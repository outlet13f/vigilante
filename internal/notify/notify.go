// Package notify sends best-effort alerts (Slack-compatible incoming webhooks
// or a generic JSON webhook for an in-house paging system). Notification
// failures are logged and never block a rollback.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/model"
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
}

type Notifier struct {
	targets []config.Notifier
	client  *http.Client
	log     *slog.Logger
}

func New(targets []config.Notifier, log *slog.Logger) *Notifier {
	return &Notifier{targets: targets, client: &http.Client{Timeout: 5 * time.Second}, log: log}
}

func (n *Notifier) Send(ctx context.Context, m Message) {
	if n == nil {
		return
	}
	m.LevelName = m.Level.String()
	for _, t := range n.targets {
		if m.Level < parseLevel(t.MinLevel) {
			continue
		}
		url := t.URL
		if t.URLEnv != "" {
			url = os.Getenv(t.URLEnv)
		}
		if url == "" {
			continue
		}
		var payload any = m
		if t.Type == "slack" {
			icon := map[Level]string{Info: ":information_source:", Warning: ":warning:", Critical: ":rotating_light:"}[m.Level]
			payload = map[string]string{"text": icon + " *" + m.Title + "*\n" + m.Text}
		}
		b, _ := json.Marshal(payload)
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		req, err := http.NewRequestWithContext(cctx, http.MethodPost, url, bytes.NewReader(b))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			var resp *http.Response
			if resp, err = n.client.Do(req); err == nil {
				resp.Body.Close()
			}
		}
		cancel()
		if err != nil {
			n.log.Warn("notification failed", "type", t.Type, "err", err)
		}
	}
	n.log.Info("notify", "level", m.Level.String(), "title", m.Title, "text", m.Text)
}
