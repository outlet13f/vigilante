package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/model"
)

type sink struct {
	mu   sync.Mutex
	got  []map[string]any
	srv  *httptest.Server
	code int
}

func newSink(t *testing.T) *sink {
	s := &sink{code: 200}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		s.mu.Lock()
		s.got = append(s.got, m)
		s.mu.Unlock()
		w.WriteHeader(s.code)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *sink) n() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRoutingDedupAndChannelFormats(t *testing.T) {
	slack, teams, pay, pd := newSink(t), newSink(t), newSink(t), newSink(t)
	t.Setenv("VGL_PD_KEY", "R0UT1NGKEY")
	n := New([]config.Notifier{
		{Type: "slack", URL: slack.srv.URL, MinLevel: "warning"},
		{Type: "teams", URL: teams.srv.URL},
		{Type: "webhook", URL: pay.srv.URL, Teams: []string{"payments"}},
		{Type: "pagerduty", RoutingKeyRef: "env:VGL_PD_KEY", MinLevel: "critical"},
	}, quiet())
	n.PagerDutyURL = pd.srv.URL
	n.TeamOf = func(svc string) string { return map[string]string{"order": "payments"}[svc] }
	now := time.Now()
	n.Now = func() time.Time { return now }
	ctx := context.Background()
	order := &model.Deployment{ID: "d1", Service: "order", Version: "v2", PreviousVersion: "v1", State: model.StateRollbackFailed}
	search := &model.Deployment{ID: "d2", Service: "search", Version: "s2", PreviousVersion: "s1"}

	n.Send(ctx, Message{Level: Info, Title: "deployment created", Deployment: order})
	n.Send(ctx, Message{Level: Critical, Title: "ROLLBACK FAILED order", Text: "drained", Deployment: order})
	n.Send(ctx, Message{Level: Critical, Title: "ROLLBACK FAILED order", Text: "drained", Deployment: order}) // duplicate
	n.Send(ctx, Message{Level: Warning, Title: "search held", Deployment: search})

	if slack.n() != 2 || teams.n() != 3 || pay.n() != 2 || pd.n() != 1 {
		t.Fatalf("deliveries slack=%d teams=%d payments-webhook=%d pagerduty=%d", slack.n(), teams.n(), pay.n(), pd.n())
	}
	if txt := slack.got[0]["text"].(string); !strings.Contains(txt, ":rotating_light:") || !strings.Contains(txt, "ROLLBACK FAILED") {
		t.Fatalf("slack: %v", slack.got[0])
	}
	card := teams.got[1]["attachments"].([]any)[0].(map[string]any)
	if card["contentType"] != "application/vnd.microsoft.card.adaptive" ||
		!strings.Contains(mustJSON(card), `"Attention"`) || !strings.Contains(mustJSON(card), "v1 → v2") {
		t.Fatalf("teams card: %s", mustJSON(card))
	}
	ev := pd.got[0]
	if ev["routing_key"] != "R0UT1NGKEY" || ev["event_action"] != "trigger" || ev["payload"].(map[string]any)["severity"] != "critical" ||
		!strings.Contains(ev["dedup_key"].(string), "d1") {
		t.Fatalf("pagerduty: %v", ev)
	}
	// After the dedup window the same alert goes out again.
	now = now.Add(DedupWindow + time.Second)
	n.Send(ctx, Message{Level: Critical, Title: "ROLLBACK FAILED order", Deployment: order})
	if pd.n() != 2 {
		t.Fatal("alert after the dedup window was suppressed")
	}
	// Global alerts (no service) reach team-routed channels too.
	n.Send(ctx, Message{Level: Critical, Title: "circuit OPEN"})
	if pay.n() != 4 {
		t.Fatalf("global alert not routed to the team channel: %d", pay.n())
	}
}

// fakeSMTP is a minimal SMTP server that records one message.
func fakeSMTP(t *testing.T) (addr string, got chan string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		say := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
		say("220 fake ESMTP")
		var data strings.Builder
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if inData {
				if line == ".\r\n" {
					inData = false
					got <- data.String()
					say("250 queued")
					continue
				}
				data.WriteString(line)
				continue
			}
			switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				say("250-fake\r\n250 AUTH PLAIN")
			case strings.HasPrefix(cmd, "AUTH PLAIN"):
				say("235 ok")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				say("250 ok")
			case cmd == "DATA":
				inData = true
				say("354 go")
			case cmd == "QUIT":
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	return ln.Addr().String(), got
}

func TestEmail(t *testing.T) {
	addr, got := fakeSMTP(t)
	host, port, _ := net.SplitHostPort(addr)
	p, _ := strconv.Atoi(port)
	t.Setenv("VGL_SMTP_PW", "mail-password")
	n := New([]config.Notifier{{Type: "email", SMTP: &config.SMTP{Host: host, Port: p, From: "vigilante@example.internal",
		To: []string{"sre@example.internal", "pay@example.internal"}, Username: "vigilante", PasswordRef: "env:VGL_SMTP_PW", NoStartTLS: true}}}, quiet())
	n.Send(context.Background(), Message{Level: Critical, Title: "롤백 실패 order", Text: "line one\nline two",
		Deployment: &model.Deployment{ID: "d1", Service: "order", Version: "v2", PreviousVersion: "v1", State: model.StateRollbackFailed}})
	select {
	case msg := <-got:
		for _, want := range []string{"To: sre@example.internal, pay@example.internal", "Subject: =?utf-8?q?", "line one\r\nline two", "Deployment: d1", "State: ROLLBACK_FAILED"} {
			if !strings.Contains(msg, want) {
				t.Errorf("mail lacks %q:\n%s", want, msg)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no mail delivered")
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
