package audit

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/journal"
	"vigilante/internal/model"
	"vigilante/internal/telemetry"
	"vigilante/internal/tlsconf"
)

// Exporter ships audit-relevant journal entries to a SIEM over syslog
// (RFC 5424 with a JSON message, or CEF). Sending never blocks the engine: a
// full queue drops entries and counts them; the journal stays the record of
// truth and `vigilante audit export` can backfill.
//
// udp and tcp send one message per line; tls (RFC 5425) sends octet-counted
// frames, "LEN SP MSG", over a verified TLS connection.
type Exporter struct {
	network, addr string
	tls           *tls.Config // network "tls"
	format        string
	host          string
	log           *slog.Logger
	ch            chan journal.Entry
	Dropped       atomic.Int64
	Sent          atomic.Int64

	mu        sync.Mutex
	lastState map[string]model.State // deployment -> last exported state
}

// NewExporter parses address "tcp://host:port", "udp://host:port" or
// "tls://host[:port]" (default port 6514).
func NewExporter(cfg config.SyslogExport, log *slog.Logger) (*Exporter, error) {
	network, addr, ok := strings.Cut(cfg.Address, "://")
	if !ok || (network != "tcp" && network != "udp" && network != "tls") || addr == "" {
		return nil, fmt.Errorf("audit.syslog.address must be tcp://host:port, udp://host:port or tls://host[:port], got %q", cfg.Address)
	}
	format := cfg.Format
	if format == "" {
		format = "rfc5424"
	}
	x := &Exporter{network: network, addr: addr, format: format, log: log,
		ch: make(chan journal.Entry, 10000), lastState: map[string]model.State{}}
	if network == "tls" {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			x.addr = net.JoinHostPort(strings.Trim(addr, "[]"), "6514")
		}
		host, _, _ := net.SplitHostPort(x.addr)
		tc, err := tlsconf.Syslog(cfg.TLS, host)
		if err != nil {
			return nil, err
		}
		x.tls = tc
	} else if cfg.TLS != nil {
		return nil, errors.New("audit.syslog.tls needs a tls:// address")
	}
	x.host, _ = os.Hostname()
	return x, nil
}

// dial connects to the collector; tls includes the handshake in the timeout.
func (x *Exporter) dial(ctx context.Context) (net.Conn, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	if x.tls != nil {
		td := tls.Dialer{NetDialer: &d, Config: x.tls}
		return td.DialContext(ctx, "tcp", x.addr)
	}
	return d.DialContext(ctx, x.network, x.addr)
}

// frame wraps one message for the transport.
func (x *Exporter) frame(msg string) []byte {
	if x.tls != nil {
		return []byte(strconv.Itoa(len(msg)) + " " + msg) // RFC 5425 octet counting
	}
	return []byte(msg + "\n")
}

// Send queues an entry if it is audit-relevant.
func (x *Exporter) Send(e journal.Entry) {
	switch e.Kind {
	case journal.KindStepDone, journal.KindIdempotency, journal.KindOperation, journal.KindAPIClient, journal.KindAccessToken, journal.KindClientUsed,
		journal.KindEvent, journal.KindWebhook, journal.KindWebhookCursor, journal.KindFreeze:
		return // bookkeeping: the matching audit and deployment entries carry the story
	case journal.KindDeployment:
		if e.Deployment == nil {
			return
		}
		x.mu.Lock()
		changed := x.lastState[e.Deployment.ID] != e.Deployment.State
		x.lastState[e.Deployment.ID] = e.Deployment.State
		x.mu.Unlock()
		if !changed {
			return
		}
	}
	select {
	case x.ch <- e:
	default:
		x.Dropped.Add(1)
		telemetry.AuditDropped.Inc()
	}
}

// Run delivers queued entries until ctx ends, reconnecting with backoff.
func (x *Exporter) Run(ctx context.Context) {
	var conn net.Conn
	backoff := time.Second
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()
	for {
		var e journal.Entry
		select {
		case <-ctx.Done():
			return
		case e = <-x.ch:
		}
		line := x.Format(e)
		for attempt := 0; ; attempt++ {
			if conn == nil {
				var err error
				if conn, err = x.dial(ctx); err != nil {
					conn = nil
					x.log.Warn("SIEM unreachable; audit entries queue up", "addr", x.addr, "err", err, "queued", len(x.ch))
					select {
					case <-ctx.Done():
						return
					case <-time.After(backoff):
					}
					backoff = min(backoff*2, time.Minute)
					continue
				}
				backoff = time.Second
			}
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := conn.Write(x.frame(line)); err != nil {
				conn.Close()
				conn = nil
				if attempt < 1 {
					continue
				}
				x.Dropped.Add(1)
				telemetry.AuditDropped.Inc()
				break
			}
			x.Sent.Add(1)
			telemetry.AuditExported.Inc()
			break
		}
	}
}

// Format renders one entry in the configured format.
func (x *Exporter) Format(e journal.Entry) string {
	if x.format == "cef" {
		return FormatCEF(e)
	}
	return FormatRFC5424(e, x.host)
}

// severity: 2 critical, 4 warning, 5 notice, 6 info.
func severity(e journal.Entry) int {
	switch {
	case e.Action == "denied":
		return 4
	case e.Kind == journal.KindCircuit && e.Circuit != nil && e.Circuit.State == "OPEN":
		return 2
	case e.Kind == journal.KindDeployment && e.Deployment != nil && e.Deployment.State == model.StateRollbackFailed:
		return 2
	case e.Kind == journal.KindAudit, e.Kind == journal.KindRollbackStart, e.Kind == journal.KindAnchor:
		return 5
	}
	return 6
}

// summary is the JSON message body: small, flat, no full deployment dumps.
func summary(e journal.Entry) map[string]any {
	m := map[string]any{"time": e.Time.UTC().Format(time.RFC3339Nano), "kind": e.Kind}
	add := func(k, v string) {
		if v != "" {
			m[k] = v
		}
	}
	add("actor", e.Actor)
	add("source", e.Source)
	add("action", e.Action)
	add("reason", e.Reason)
	add("ticket", e.Ticket)
	add("message", e.Message)
	add("service", e.Service)
	add("deployment_id", e.DeployID)
	if d := e.Deployment; d != nil {
		add("deployment_id", d.ID)
		add("service", d.Service)
		m["state"] = string(d.State)
		add("version", d.Version)
		add("previous_version", d.PreviousVersion)
		if e.Reason == "" {
			add("reason", d.Reason)
		}
	}
	if c := e.Circuit; c != nil {
		m["circuit"] = string(c.State)
		add("reason", c.Reason)
	}
	return m
}

func eventName(e journal.Entry) string {
	if e.Action != "" {
		return e.Action
	}
	if e.Kind == journal.KindDeployment && e.Deployment != nil {
		return "deployment." + strings.ToLower(string(e.Deployment.State))
	}
	return e.Kind
}

// FormatRFC5424 renders `<PRI>1 TIMESTAMP HOST vigilante - MSGID [SD] JSON`
// with facility 13 (log audit).
func FormatRFC5424(e journal.Entry, host string) string {
	pri := 13*8 + severity(e)
	m := summary(e)
	sd := fmt.Sprintf(`[vigilante@32473 actor="%s" service="%s" deployment="%s"]`,
		sdEscape(str(m["actor"])), sdEscape(str(m["service"])), sdEscape(str(m["deployment_id"])))
	body, _ := json.Marshal(m)
	return fmt.Sprintf("<%d>1 %s %s vigilante - %s %s %s", pri, e.Time.UTC().Format(time.RFC3339Nano),
		nilDash(host), nilDash(eventName(e)), sd, body)
}

// FormatCEF renders an ArcSight Common Event Format line.
func FormatCEF(e journal.Entry) string {
	m := summary(e)
	sev := map[int]int{2: 9, 4: 6, 5: 4, 6: 2}[severity(e)]
	name := str(m["message"])
	if name == "" {
		name = str(m["reason"])
	}
	if name == "" {
		name = eventName(e)
	}
	ext := []string{fmt.Sprintf("rt=%d", e.Time.UnixMilli())}
	kv := func(k, v string) {
		if v != "" {
			ext = append(ext, k+"="+cefExt(v))
		}
	}
	kv("suser", str(m["actor"]))
	kv("act", eventName(e))
	kv("cs1Label", "service")
	kv("cs1", str(m["service"]))
	kv("cs2Label", "deployment")
	kv("cs2", str(m["deployment_id"]))
	kv("cs3Label", "state")
	kv("cs3", str(m["state"]))
	kv("cs4Label", "ticket")
	kv("cs4", str(m["ticket"]))
	kv("msg", str(m["reason"]))
	return fmt.Sprintf("CEF:0|Vigilante|vigilante|1|%s|%s|%d|%s",
		cefHeader(eventName(e)), cefHeader(name), sev, strings.Join(ext, " "))
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func nilDash(s string) string {
	if s == "" {
		return "-"
	}
	return strings.ReplaceAll(s, " ", "_")
}

func sdEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, `]`, `\]`).Replace(s)
}

func cefHeader(s string) string {
	return strings.NewReplacer(`\`, `\\`, `|`, `\|`, "\n", " ").Replace(s)
}

func cefExt(s string) string {
	return strings.NewReplacer(`\`, `\\`, `=`, `\=`, "\n", `\n`).Replace(s)
}
