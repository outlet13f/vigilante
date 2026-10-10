// Package notify sends best-effort alerts: Slack and Microsoft Teams
// incoming webhooks, email over SMTP, PagerDuty Events v2, or a generic
// JSON webhook for an in-house paging system. Each channel can be routed to
// services or teams and filtered by severity; the same alert for the same
// deployment is sent at most once per DedupWindow. Failures are logged and
// never block a rollback.
package notify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/model"
	"vigilante/internal/secrets"
)

type Level int

const (
	Info Level = iota
	Warning
	Critical
)

func (l Level) String() string { return [...]string{"info", "warning", "critical"}[l] }

func parseLevel(s string) Level {
	switch s {
	case "warning":
		return Warning
	case "critical":
		return Critical
	}
	return Info
}

type Message struct {
	Level      Level             `json:"-"`
	LevelName  string            `json:"level"`
	Title      string            `json:"title"`
	Text       string            `json:"text"`
	Deployment *model.Deployment `json:"deployment,omitempty"`
	// Service names the service when there is no deployment (routing).
	Service string `json:"service,omitempty"`
}

// DedupWindow suppresses repeats of the same alert.
const DedupWindow = 10 * time.Minute

type Notifier struct {
	targets []config.Notifier
	client  *http.Client
	log     *slog.Logger
	// TeamOf maps a service to its team for team routing.
	TeamOf func(service string) string
	// PagerDutyURL overrides the Events v2 endpoint (tests).
	PagerDutyURL string
	Now          func() time.Time

	mu   sync.Mutex
	sent map[string]time.Time
}

func New(targets []config.Notifier, log *slog.Logger) *Notifier {
	return &Notifier{targets: targets, client: &http.Client{Timeout: 5 * time.Second}, log: log,
		PagerDutyURL: "https://events.pagerduty.com/v2/enqueue", Now: time.Now, sent: map[string]time.Time{}}
}

func (m *Message) service() string {
	if m.Deployment != nil {
		return m.Deployment.Service
	}
	return m.Service
}

func (n *Notifier) routed(t config.Notifier, m *Message) bool {
	if m.Level < parseLevel(t.MinLevel) {
		return false
	}
	if len(t.Services) == 0 && len(t.Teams) == 0 {
		return true
	}
	svc := m.service()
	if svc == "" {
		return true // global alerts (circuit breaker) go everywhere
	}
	team := ""
	if n.TeamOf != nil {
		team = n.TeamOf(svc)
	}
	return slices.Contains(t.Services, svc) || (team != "" && slices.Contains(t.Teams, team))
}

// dedupKey identifies an alert: same channel, deployment, level and title.
func dedupKey(i int, m *Message) string {
	id := ""
	if m.Deployment != nil {
		id = m.Deployment.ID
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%d|%s", i, id, m.Level, m.Title)))
	return hex.EncodeToString(sum[:8])
}

func (n *Notifier) duplicate(key string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.Now()
	for k, t := range n.sent {
		if now.Sub(t) > DedupWindow {
			delete(n.sent, k)
		}
	}
	if t, ok := n.sent[key]; ok && now.Sub(t) < DedupWindow {
		return true
	}
	n.sent[key] = now
	return false
}

func (n *Notifier) Send(ctx context.Context, m Message) {
	if n == nil {
		return
	}
	m.LevelName = m.Level.String()
	for i, t := range n.targets {
		if !n.routed(t, &m) || n.duplicate(dedupKey(i, &m)) {
			continue
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		err := n.deliver(cctx, t, &m)
		cancel()
		if err != nil {
			n.log.Warn("notification failed", "type", t.Type, "err", err)
		}
	}
	n.log.Info("notify", "level", m.Level.String(), "title", m.Title, "text", m.Text)
}

func (n *Notifier) deliver(ctx context.Context, t config.Notifier, m *Message) error {
	switch t.Type {
	case "email":
		return n.email(ctx, t, m)
	case "pagerduty":
		return n.pagerduty(ctx, t, m)
	}
	url := t.URL
	if t.URLEnv != "" {
		url = os.Getenv(t.URLEnv)
	}
	if t.URLRef != "" {
		var err error
		if url, err = secrets.Resolve(ctx, t.URLRef); err != nil {
			return err
		}
	}
	if url == "" {
		return nil
	}
	var payload any = m
	switch t.Type {
	case "slack":
		icon := map[Level]string{Info: ":information_source:", Warning: ":warning:", Critical: ":rotating_light:"}[m.Level]
		payload = map[string]string{"text": icon + " *" + m.Title + "*\n" + m.Text}
	case "teams":
		payload = teamsCard(m)
	}
	return n.postJSON(ctx, url, payload)
}

func (n *Notifier) postJSON(ctx context.Context, url string, payload any) error {
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s answered %d: %s", url, resp.StatusCode, bytes.TrimSpace(body))
	}
	return nil
}

// teamsCard is an Adaptive Card message for a Teams Workflows webhook.
func teamsCard(m *Message) map[string]any {
	color := map[Level]string{Info: "Default", Warning: "Warning", Critical: "Attention"}[m.Level]
	body := []map[string]any{
		{"type": "TextBlock", "text": m.Title, "weight": "Bolder", "size": "Medium", "color": color, "wrap": true},
		{"type": "TextBlock", "text": m.Text, "wrap": true},
	}
	if d := m.Deployment; d != nil {
		body = append(body, map[string]any{"type": "FactSet", "facts": []map[string]string{
			{"title": "Service", "value": d.Service}, {"title": "Deployment", "value": d.ID},
			{"title": "Version", "value": d.PreviousVersion + " → " + d.Version}, {"title": "State", "value": string(d.State)},
		}})
	}
	return map[string]any{"type": "message", "attachments": []map[string]any{{
		"contentType": "application/vnd.microsoft.card.adaptive",
		"content":     map[string]any{"$schema": "http://adaptivecards.io/schemas/adaptive-card.json", "type": "AdaptiveCard", "version": "1.4", "body": body},
	}}}
}

// pagerduty triggers a PagerDuty Events v2 alert; dedup_key keeps one
// incident per deployment and title.
func (n *Notifier) pagerduty(ctx context.Context, t config.Notifier, m *Message) error {
	key, err := secrets.Value(ctx, t.RoutingKeyRef, t.RoutingKeyEnv)
	if err != nil || key == "" {
		return fmt.Errorf("pagerduty routing key: %v", err)
	}
	dedup := "vigilante:" + m.Title
	source := "vigilante"
	if d := m.Deployment; d != nil {
		dedup, source = "vigilante:"+d.ID+":"+m.Title, d.Service
	}
	return n.postJSON(ctx, n.PagerDutyURL, map[string]any{
		"routing_key": key, "event_action": "trigger", "dedup_key": dedup,
		"payload": map[string]any{"summary": truncate(m.Title+": "+m.Text, 1024), "source": source,
			"severity":  map[Level]string{Info: "info", Warning: "warning", Critical: "critical"}[m.Level],
			"component": "vigilante", "custom_details": map[string]any{"text": m.Text, "deployment": m.Deployment}},
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
