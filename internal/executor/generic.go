package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/transport"
)

func init() {
	Register("exec", newExec)
	Register("webhook", newWebhook)
}

// exec: arbitrary commands — the escape hatch for in-house deploy tooling
// (Ansible playbooks, a release script, `kubectl rollout undo` from a jump box).
type execExec struct{ spec *config.ExecExec }

func newExec(_ string, spec config.Executor) (Executor, error) {
	return &execExec{spec: spec.Exec}, nil
}

func (e *execExec) runner(rc *RunContext) (transport.Runner, error) {
	switch on := e.spec.OnHost; on {
	case "", "target":
		return rc.runner()
	case "local":
		return rc.wrap(&transport.Local{}), nil
	default:
		return rc.runnerFor(on)
	}
}

func (e *execExec) run(ctx context.Context, rc *RunContext, tmplStr string) (string, error) {
	if tmplStr == "" {
		return "", nil
	}
	r, err := e.runner(rc)
	if err != nil {
		return "", err
	}
	cmd, err := rc.render(tmplStr)
	if err != nil {
		return "", err
	}
	return r.Run(ctx, cmd, nil)
}

func (e *execExec) Prepare(ctx context.Context, rc *RunContext) (map[string]string, error) {
	out, err := e.run(ctx, rc, e.spec.Prepare)
	if err != nil {
		return nil, err
	}
	return map[string]string{"exec.prepare_output": strings.TrimSpace(out)}, nil
}

func (e *execExec) Rollback(ctx context.Context, rc *RunContext) error {
	_, err := e.run(ctx, rc, e.spec.Rollback)
	return err
}

func (e *execExec) Verify(ctx context.Context, rc *RunContext) error {
	if rc.DryRun {
		return nil
	}
	_, err := e.run(ctx, rc, e.spec.Verify)
	return err
}

// webhook: hand the rollback to an external deploy console / CD system.
type webhookExec struct {
	spec   *config.WebhookExec
	client *http.Client
}

func newWebhook(_ string, spec config.Executor) (Executor, error) {
	return &webhookExec{spec: spec.Webhook, client: &http.Client{Timeout: 60 * time.Second}}, nil
}

func (w *webhookExec) send(ctx context.Context, rc *RunContext, method, urlTmpl, bodyTmpl string) error {
	u, err := rc.render(urlTmpl)
	if err != nil {
		return err
	}
	body, err := rc.render(bodyTmpl)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range w.spec.Headers {
		hv, err := rc.render(v)
		if err != nil {
			return err
		}
		req.Header.Set(k, hv)
	}
	if tok := bearer(rc.Creds, w.spec.Credential); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	} else if user, pass, _ := basicAuth(rc.Creds, w.spec.Credential); user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, u, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

func (w *webhookExec) Rollback(ctx context.Context, rc *RunContext) error {
	if rc.DryRun {
		u, _ := rc.render(w.spec.URL)
		rc.Log.Info("DRY-RUN: would call rollback webhook", "url", u)
		return nil
	}
	return w.send(ctx, rc, w.spec.Method, w.spec.URL, w.spec.Body)
}

func (w *webhookExec) Verify(ctx context.Context, rc *RunContext) error {
	if rc.DryRun || w.spec.VerifyURL == "" {
		return nil
	}
	return w.send(ctx, rc, http.MethodGet, w.spec.VerifyURL, "")
}
