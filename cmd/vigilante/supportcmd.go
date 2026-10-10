package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"vigilante/internal/audit"
	"vigilante/internal/compat"
	"vigilante/internal/config"
	"vigilante/internal/doctor"
	"vigilante/internal/model"
	"vigilante/internal/store"
	"vigilante/internal/support"
	"vigilante/internal/transport"
)

// maxLog is how much of each log is kept (the end).
const maxLog = 20 << 20

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// cmdSupportBundle collects diagnostics into a zip with secrets removed. It
// keeps going when parts fail (a broken config is a common reason to run
// it) and never changes anything: no schema migration, no rollback.
func cmdSupportBundle(ctx context.Context, args []string) (int, error) {
	c := newFlags("support-bundle")
	host, _ := os.Hostname()
	now := time.Now()
	out := c.fs.String("out", fmt.Sprintf("vigilante-support-%s-%s.zip", host, now.UTC().Format("20060102T150405Z")), "output zip file")
	noDoctor := c.fs.Bool("no-doctor", false, "skip the read-only doctor checks against targets")
	doctorTimeout := c.fs.Duration("doctor-timeout", 10*time.Second, "timeout per doctor check")
	noVerify := c.fs.Bool("no-verify", false, "skip verifying the audit hash chain (slow on large journals)")
	since := c.fs.Duration("since", 24*time.Hour, "how much of the systemd journal to include")
	var logs stringList
	c.fs.Var(&logs, "log", "log file to include (repeatable; the last 20 MB of each)")
	if err := c.fs.Parse(args); err != nil {
		return 1, err
	}
	f, err := os.OpenFile(*out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 1, err
	}
	defer f.Close()
	b := support.New(f, now)

	// Build and host.
	var plugins bytes.Buffer
	printPlugins(&plugins)
	_ = b.Add("version.txt", []byte(fmt.Sprintf("%s\nhost: %s (%s/%s, %d CPUs)\n\n%s", buildInfo(), host, runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), plugins.String())))
	_ = b.Add("env.txt", []byte(envNames()))

	// Configuration (redacted) and its validation.
	cfg := collectConfig(b, c.config)

	if cfg != nil {
		collectState(ctx, b, cfg, !*noVerify)
		if *noDoctor {
			b.Note("doctor checks (--no-doctor)")
		} else {
			collectDoctor(ctx, b, cfg, *doctorTimeout)
		}
	}
	if c.srv != "" {
		collectServer(ctx, b, c.srv)
	} else {
		b.Note("running server's /healthz, /readyz and /metrics (pass --server URL, token in VIGILANTE_TOKEN)")
	}
	collectLogs(ctx, b, logs, *since)

	if err := b.Close(buildInfo()); err != nil {
		return 1, err
	}
	fmt.Printf("wrote %s\nSecrets were replaced with %s; look through the files before sending them.\n", *out, support.Redacted)
	return 0, nil
}

func envNames() string {
	var names []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "VIGILANTE_") || strings.HasPrefix(k, "VAULT_") {
			names = append(names, k+" (set; value not collected)")
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "no VIGILANTE_* or VAULT_* variables set\n"
	}
	return strings.Join(names, "\n") + "\n"
}

func collectConfig(b *support.Bundle, path string) *config.Config {
	raw, err := os.ReadFile(path)
	if err != nil {
		b.Note("configuration %s: %v", path, err)
		return nil
	}
	if red, n, err := support.RedactYAML(raw); err != nil {
		// Unparsable YAML cannot be redacted reliably: leave it out.
		b.Note("configuration file (not valid YAML, so it was left out rather than risk secrets: %v)", err)
	} else {
		_ = b.Add("config/vigilante.yaml", append([]byte(fmt.Sprintf("# %s, %d secret values redacted\n", path, n)), red...))
	}
	var v strings.Builder
	cfg, err := loadConfig(path)
	if err != nil {
		v.WriteString("INVALID: " + err.Error() + "\n")
	} else {
		v.WriteString(fmt.Sprintf("OK: %d targets, %d services, %d executors, %d traffic controllers\n", len(cfg.Targets), len(cfg.Services), len(cfg.Executors), len(cfg.Traffic)))
		for _, w := range cfg.Warnings() {
			v.WriteString("WARN: " + w + "\n")
		}
		if w := compat.Warning(cfg); w != "" {
			v.WriteString("WARN: " + w + "\n")
		}
	}
	_ = b.Add("config/validate.txt", []byte(v.String()))
	return cfg
}

func collectState(ctx context.Context, b *support.Bundle, cfg *config.Config, verify bool) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ro := *cfg
	if ro.Server.State.Backend == "postgres" {
		off := false
		ro.Server.State.AutoMigrate = &off // never migrate from here
		if dsn, err := store.PostgresDSN(ctx, &ro); err != nil {
			b.Note("schema status: %v", err)
		} else if s, err := store.Schema(ctx, dsn); err != nil {
			b.Note("schema status: %v", err)
		} else {
			_ = b.AddJSON("state/schema.json", s)
		}
	}
	if be := ro.Server.State.Backend; be == "" || be == "file" {
		// Opening a missing journal would create it (and its directories).
		if _, err := os.Stat(ro.Server.JournalPath); err != nil {
			b.Note("state store: journal %s: %v", ro.Server.JournalPath, err)
			return
		}
	}
	st, err := store.Open(ctx, &ro)
	if err != nil {
		b.Note("state store: %v", err)
		return
	}
	defer st.Close()
	s, err := st.Load(ctx)
	if err != nil {
		b.Note("state store %s: %v", st.Describe(), err)
		return
	}
	byState := map[string]int{}
	type dep struct {
		ID, Service, Version, State, Phase, Reason string
		Updated                                    time.Time
	}
	var open []dep
	for _, d := range s.Deployments {
		byState[string(d.State)]++
		if !d.State.Terminal() || d.State == model.StateRollbackFailed {
			open = append(open, dep{d.ID, d.Service, d.Version, string(d.State), string(d.Phase), trim(d.Reason, 300), d.UpdatedAt})
		}
	}
	sort.Slice(open, func(i, j int) bool { return open[i].Updated.After(open[j].Updated) })
	active := 0
	for _, fz := range s.Freezes {
		if fz.EndedAt == nil && fz.EndsAt.After(time.Now()) {
			active++
		}
	}
	_ = b.AddJSON("state/summary.json", map[string]any{
		"store": st.Describe(), "unreadable_entries": s.Corrupt,
		"deployments_by_state": byState, "open_deployments": open,
		"circuit": s.Circuit, "rollbacks_in_flight": len(s.InFlight()),
		"api_clients": len(s.Clients), "webhooks": len(s.Webhooks), "declared_freezes_in_force": active,
	})
	if verify {
		key, err := store.ChainKey(ctx, &ro) // resolved once already by store.Open
		var r audit.Report
		if err == nil {
			r, err = audit.Verify(ctx, st, key)
		}
		if err != nil {
			b.Note("audit chain verification: %v", err)
		} else {
			_ = b.AddJSON("state/audit-verify.json", r)
		}
	} else {
		b.Note("audit chain verification (--no-verify)")
	}
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func collectDoctor(ctx context.Context, b *support.Bundle, cfg *config.Config, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	mgr := transport.NewManager(cfg)
	defer mgr.Close()
	checks, err := doctor.Run(ctx, cfg, doctor.Options{Timeout: timeout, Runners: mgr.ForTarget, Log: logger()})
	if err != nil {
		b.Note("doctor: %v", err)
		return
	}
	var txt bytes.Buffer
	printChecks(&txt, checks)
	_ = b.Add("doctor.txt", txt.Bytes())
	_ = b.AddJSON("doctor.json", checks)
}

func collectServer(ctx context.Context, b *support.Bundle, base string) {
	client := &http.Client{Timeout: 15 * time.Second}
	token := os.Getenv("VIGILANTE_TOKEN")
	for _, p := range []struct{ path, file string }{
		{"/healthz", "server/healthz.json"}, {"/readyz", "server/readyz.json"},
		{"/metrics", "server/metrics.txt"}, {"/v2/circuit", "server/circuit.json"},
	} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+p.path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			b.Note("%s: %v", p.path, err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxLog))
		resp.Body.Close()
		_ = b.Add(p.file, append([]byte(fmt.Sprintf("# GET %s -> %s\n", p.path, resp.Status)), body...))
	}
}

func collectLogs(ctx context.Context, b *support.Bundle, files []string, since time.Duration) {
	for _, path := range files {
		data, err := tail(path, maxLog)
		if err != nil {
			b.Note("log %s: %v", path, err)
			continue
		}
		name := strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(strings.TrimLeft(path, "/\\"))
		_ = b.Add("logs/"+name, data)
	}
	if _, err := exec.LookPath("journalctl"); err != nil {
		if len(files) == 0 {
			b.Note("logs (no journalctl here; pass --log FILE)")
		}
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "journalctl", "--no-pager", "-o", "short-iso",
		"-u", "vigilante-server", "-u", "vigilante-agent", "--since", time.Now().Add(-since).Format("2006-01-02 15:04:05"))
	data, err := cmd.Output()
	if err != nil {
		b.Note("journalctl: %v", err)
		return
	}
	if len(data) > maxLog {
		data = data[len(data)-maxLog:]
	}
	_ = b.Add("logs/journal.txt", data)
}

func tail(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() > n {
		if _, err := f.Seek(-n, io.SeekEnd); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(f)
}
