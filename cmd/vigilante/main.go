// Command vigilante is the Unified Rollback Orchestrator: one static binary
// that acts as CLI gate for CI pipelines, central server, or on-host agent.
//
// Exit codes (watch / rollback):
//
//	0 PASS (phase promoted)   1 error   2 FAIL -> rolled back
//	3 rollback failed / circuit open / approval needed   4 HOLD / inconclusive
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"vigilante/internal/agent"
	"vigilante/internal/api"
	"vigilante/internal/audit"
	"vigilante/internal/compat"
	"vigilante/internal/config"
	"vigilante/internal/model"
	"vigilante/internal/orchestrator"
	"vigilante/internal/safety"
	"vigilante/internal/secrets"
	"vigilante/internal/store"
	"vigilante/internal/telemetry"
	"vigilante/internal/tlsconf"
)

var version = "0.1.0-dev"

const usage = `vigilante — unified rollback orchestrator

Usage:
  vigilante validate -c FILE
  vigilante prepare  -c FILE --service S --version V --previous P [--id ID]
  vigilante baseline -c FILE --service S [--window 5m] [--out baseline.json]
  vigilante watch    -c FILE --service S --phase canary|rolling|full
                     [--version V] [--previous P] [--id ID] [--baseline FILE] [--dry-run] [--server URL]
  vigilante mark-good -c FILE --service S --version V [--reason TEXT]
  vigilante rollback -c FILE (--id ID | --service S --version V --previous P [--targets a,b])
                     [--executor NAME] [--approve] [--reason TEXT] [--dry-run]
  vigilante status   -c FILE [--id ID]
  vigilante circuit  -c FILE status|reset|trip [--reason TEXT] [--server URL]
  vigilante server   -c FILE [--dry-run]
  vigilante agent    -c FILE --target NAME --server URL
  vigilante doctor   -c FILE [--service S] [--previous P] [--json] [--junit FILE]
  vigilante presets [list | show NAME[@V] [--set k=v]...] [--dir DIRS]
  vigilante token    create --name NAME [--role ROLE] [--scope SCOPE] [--expires YYYY-MM-DD]
  vigilante whoami   --server URL
  vigilante audit    verify [--file ARCHIVE] | export --out F | prune --out F (--before DATE | --older-than DUR)
                     | query [--actor A] [--action X] [--service S] [--since DATE]
  vigilante store    status | migrate [--down-to N --yes]   (PostgreSQL state store schema)
  vigilante feedback --id ID --outcome correct|false_positive|false_negative|unclear [--incident INC] [--note T] [--server URL]
  vigilante pilot    report [--since DATE] [--until DATE] [--service A,B] [--json] [--out F]   (decision quality; exit 4 = gate not met)
  vigilante support-bundle -c FILE [--server URL] [--log FILE]... [--out F.zip]   (diagnostics, secrets removed)
  vigilante plugins
  vigilante version

--id and --version default to the CI run (Jenkins, GitLab CI, GitHub Actions,
VIGILANTE_DEPLOYMENT_ID / VIGILANTE_VERSION, or git describe); --previous defaults
to the service's last successful deployment (see mark-good).

Environment: VIGILANTE_TOKEN (API token for --server / agent), VIGILANTE_LOG=debug|info|warn
`

func main() {
	telemetry.Version, telemetry.Flavor = version, flavor
	store.BinaryVersion = version
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code, err := run(ctx, os.Args[1], os.Args[2:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func logger() *slog.Logger {
	lvl := slog.LevelInfo
	switch strings.ToLower(os.Getenv("VIGILANTE_LOG")) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler = slog.NewTextHandler(os.Stderr, opts)
	if strings.EqualFold(os.Getenv("VIGILANTE_LOG_FORMAT"), "json") {
		h = slog.NewJSONHandler(os.Stderr, opts) // one object per line, for log shippers
	}
	return slog.New(secrets.RedactHandler{Handler: h})
}

// loadConfig reads vigilante.yaml and installs the secrets resolver it
// describes, so *_ref values resolve from Vault / env / file afterwards.
func loadConfig(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if err := checkBuild(cfg); err != nil {
		return nil, err
	}
	if err := secrets.Configure(cfg.Secrets); err != nil {
		return nil, err
	}
	return cfg, nil
}

type common struct {
	fs                                  *flag.FlagSet
	config, service, ver, prev, id, srv string
	ticket                              string
	dryRun                              bool
}

func newFlags(name string) *common {
	c := &common{fs: flag.NewFlagSet(name, flag.ContinueOnError)}
	c.fs.StringVar(&c.config, "c", "vigilante.yaml", "config file")
	c.fs.StringVar(&c.service, "service", "", "service name")
	c.fs.StringVar(&c.ver, "version", "", "new version")
	c.fs.StringVar(&c.prev, "previous", "", "previous (known-good) version")
	c.fs.StringVar(&c.id, "id", "", "deployment id (e.g. CI pipeline id)")
	c.fs.StringVar(&c.srv, "server", "", "delegate to a running orchestrator at URL")
	c.fs.BoolVar(&c.dryRun, "dry-run", false, "log actions instead of executing them")
	c.fs.StringVar(&c.ticket, "ticket", "", "change or incident ticket recorded in the audit trail")
	return c
}

func (c *common) engine() (*orchestrator.Engine, error) {
	cfg, err := loadConfig(c.config)
	if err != nil {
		return nil, err
	}
	return orchestrator.New(cfg, orchestrator.Options{DryRun: c.dryRun, Log: logger()})
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func run(ctx context.Context, cmd string, args []string) (int, error) {
	switch cmd {
	case "version", "--version":
		fmt.Println(buildInfo())
		return 0, nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return 0, nil
	case "plugins":
		printPlugins(os.Stdout)
		return 0, nil
	case "validate":
		c := newFlags(cmd)
		if err := c.fs.Parse(args); err != nil {
			return 1, err
		}
		cfg, err := loadConfig(c.config)
		if err != nil {
			return 1, err
		}
		fmt.Printf("OK: %d targets, %d services, %d executors, %d traffic controllers\n", len(cfg.Targets), len(cfg.Services), len(cfg.Executors), len(cfg.Traffic))
		for _, w := range cfg.Warnings() {
			fmt.Println("WARN: " + w)
		}
		if w := compat.Warning(cfg); w != "" {
			fmt.Println("WARN: " + w)
		}
		for _, s := range cfg.Services {
			if s.Preset != "" {
				fmt.Printf("  %s: preset %s -> %d probes, %d rules\n", s.Name, s.Preset, len(s.Probes), len(s.Rules))
			}
		}
		return 0, nil
	case "prepare":
		return cmdPrepare(ctx, args)
	case "baseline":
		return cmdBaseline(ctx, args)
	case "watch":
		return cmdWatch(ctx, args)
	case "rollback":
		return cmdRollback(ctx, args)
	case "status":
		return cmdStatus(args)
	case "mark-good":
		return cmdMarkGood(args)
	case "presets":
		return cmdPresets(args)
	case "doctor":
		return cmdDoctor(ctx, args)
	case "token":
		return cmdToken(args)
	case "audit":
		return cmdAudit(ctx, args)
	case "store":
		return cmdStore(ctx, args)
	case "support-bundle":
		return cmdSupportBundle(ctx, args)
	case "feedback":
		return cmdFeedback(ctx, args)
	case "pilot":
		return cmdPilot(ctx, args)
	case "whoami":
		return cmdWhoami(ctx, args)
	case "circuit":
		return cmdCircuit(ctx, args)
	case "server":
		return cmdServer(ctx, args)
	case "agent":
		return cmdAgent(ctx, args)
	}
	fmt.Fprint(os.Stderr, usage)
	return 1, fmt.Errorf("unknown command %q", cmd)
}

func cmdPrepare(ctx context.Context, args []string) (int, error) {
	c := newFlags("prepare")
	override := c.fs.String("freeze-override", "", "reason to proceed during a change freeze (recorded in the audit trail)")
	if err := c.fs.Parse(args); err != nil {
		return 1, err
	}
	e, err := c.engine()
	if err != nil {
		return 1, err
	}
	defer e.Close()
	notes := autoFill(c, e)
	d, code, err := createGated(e, c, *override)
	if err != nil {
		return code, err
	}
	annotate(e, d, notes)
	cliAudit(e, c, "deployment.prepare", d.Service, d.ID, "")
	err = e.Prepare(ctx, d)
	cp, _ := e.Deployment(d.ID)
	printJSON(cp)
	return 0, err
}

func cmdBaseline(ctx context.Context, args []string) (int, error) {
	c := newFlags("baseline")
	window := c.fs.Duration("window", 0, "observation window (default: service baseline.window)")
	out := c.fs.String("out", "", "write the baseline snapshot to this file")
	if err := c.fs.Parse(args); err != nil {
		return 1, err
	}
	e, err := c.engine()
	if err != nil {
		return 1, err
	}
	defer e.Close()
	snap, err := e.CaptureBaseline(ctx, c.service, *window)
	if snap != nil {
		if *out != "" {
			if werr := snap.Save(*out); werr != nil {
				return 1, werr
			}
		}
		printJSON(snap)
	}
	return 0, err
}

func cmdWatch(ctx context.Context, args []string) (int, error) {
	c := newFlags("watch")
	phase := c.fs.String("phase", "canary", "canary | rolling | full")
	baseline := c.fs.String("baseline", "", "baseline snapshot file from `vigilante baseline`")
	override := c.fs.String("freeze-override", "", "reason to proceed during a change freeze (recorded in the audit trail)")
	if err := c.fs.Parse(args); err != nil {
		return 1, err
	}
	if c.srv != "" {
		return watchRemote(ctx, c, *phase)
	}
	e, err := c.engine()
	if err != nil {
		return 1, err
	}
	defer e.Close()
	if err := e.LoadBaselineFile(*baseline); err != nil {
		return 1, err
	}
	notes := autoFill(c, e)
	d, code, err := createGated(e, c, *override)
	if err != nil {
		return code, err
	}
	annotate(e, d, notes)
	if err := requireRollbackTarget(d); err != nil {
		return 1, err
	}
	cliAudit(e, c, "phase.start", d.Service, d.ID, *phase)
	if err := e.Watch(ctx, d, model.Phase(*phase)); err != nil {
		e.MarkBlocked(d, err)
		if errors.Is(err, orchestrator.ErrBlocked) || errors.Is(err, safety.ErrCircuitOpen) {
			return 3, err
		}
		return 1, err
	}
	cp, _ := e.Deployment(d.ID)
	printJSON(cp)
	return model.ExitCode(cp), nil
}

func apiCall(ctx context.Context, server, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(server, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := os.Getenv("VIGILANTE_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func watchRemote(ctx context.Context, c *common, phase string) (int, error) {
	autoFill(c, nil)
	var d model.Deployment
	err := apiCall(ctx, c.srv, http.MethodPost, "/v1/deployments", map[string]any{
		"id": c.id, "service": c.service, "version": c.ver, "previous_version": c.prev, "phase": phase,
	}, &d)
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(os.Stderr, "deployment %s: observing %s via %s\n", d.ID, phase, c.srv)
	for {
		select {
		case <-ctx.Done():
			return 1, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		var st struct {
			Deployment model.Deployment `json:"deployment"`
			ExitCode   int              `json:"exit_code"`
		}
		if err := apiCall(ctx, c.srv, http.MethodGet, "/v1/deployments/"+d.ID, nil, &st); err != nil {
			fmt.Fprintln(os.Stderr, "status poll failed:", err)
			continue
		}
		switch st.Deployment.State {
		case model.StatePending, model.StateObserving, model.StateRollingBack:
			continue
		}
		printJSON(st.Deployment)
		return st.ExitCode, nil
	}
}

func cmdRollback(ctx context.Context, args []string) (int, error) {
	c := newFlags("rollback")
	exec := c.fs.String("executor", "", "override the rollback executor")
	approve := c.fs.Bool("approve", false, "approve the rollback waiting for approval (approve mode), or allow escalation steps that require approval")
	reject := c.fs.Bool("reject", false, "reject the rollback waiting for approval: the deployment is held on the new version")
	reason := c.fs.String("reason", "manual rollback (CLI)", "reason recorded in the journal")
	targets := c.fs.String("targets", "", "comma-separated targets (default: deployment targets)")
	if err := c.fs.Parse(args); err != nil {
		return 1, err
	}
	e, err := c.engine()
	if err != nil {
		return 1, err
	}
	defer e.Close()
	var d *model.Deployment
	if c.id == "" && c.service == "" {
		autoFill(c, e) // inside a pipeline: roll back this run's deployment
	}
	if c.id != "" {
		if d = e.Live(c.id); d == nil && c.service == "" {
			return 1, fmt.Errorf("deployment %q not found in journal", c.id)
		}
	}
	if d == nil {
		notes := autoFill(c, e)
		if d, err = e.Create(c.id, c.service, c.ver, c.prev); err != nil {
			return 1, err
		}
		e.SetCreatedBy(d, cliActor())
		annotate(e, d, notes)
	}
	if _, pending := e.PendingRollback(d.ID); pending && (*approve || *reject) {
		if *approve && *reject {
			return 1, errors.New("--approve and --reject are exclusive")
		}
		action := map[bool]string{true: "rollback.approve", false: "rollback.reject"}[*approve]
		cliAudit(e, c, action, d.Service, d.ID, *reason)
		_ = e.DecideRollback(ctx, d, cliActor(), *approve, *reason)
		cp, _ := e.Deployment(d.ID)
		printJSON(cp)
		return model.ExitCode(cp), nil
	}
	if *reject {
		return 1, fmt.Errorf("deployment %s has no rollback waiting for approval", d.ID)
	}
	if err := requireRollbackTarget(d); err != nil {
		return 1, err
	}
	opt := orchestrator.RollbackOptions{Reason: *reason, Manual: true, Approved: *approve, Executor: *exec, Actor: cliActor()}
	if *targets != "" {
		opt.Targets = strings.Split(*targets, ",")
	} else if len(d.Targets) == 0 {
		svc, _ := e.Cfg.Service(d.Service)
		opt.Targets = svc.Targets
	}
	cliAudit(e, c, map[bool]string{true: "escalation.approve", false: "rollback.manual"}[*approve], d.Service, d.ID, *reason)
	_ = e.Rollback(ctx, d, opt)
	cp, _ := e.Deployment(d.ID)
	printJSON(cp)
	return model.ExitCode(cp), nil
}

func annotate(e *orchestrator.Engine, d *model.Deployment, notes []string) {
	if len(notes) > 0 {
		e.Annotate(d, "input", "auto-filled "+strings.Join(notes, ", "))
	}
}

func cmdMarkGood(args []string) (int, error) {
	c := newFlags("mark-good")
	reason := c.fs.String("reason", "", "why this version is known-good")
	if err := c.fs.Parse(args); err != nil {
		return 1, err
	}
	if c.service == "" || c.ver == "" {
		return 1, errors.New("mark-good: --service and --version are required")
	}
	e, err := c.engine()
	if err != nil {
		return 1, err
	}
	defer e.Close()
	d, err := e.MarkGood(c.service, c.ver, *reason)
	if err != nil {
		return 1, err
	}
	cliAudit(e, c, "mark-good", d.Service, d.ID, c.ver)
	fmt.Printf("%s %s recorded as known-good (%s); later deployments default --previous to it\n", d.Service, d.Version, d.ID)
	return 0, nil
}

func cmdStatus(args []string) (int, error) {
	c := newFlags("status")
	if err := c.fs.Parse(args); err != nil {
		return 1, err
	}
	cfg, err := loadConfig(c.config)
	if err != nil {
		return 1, err
	}
	db, err := store.Open(context.Background(), cfg)
	if err != nil {
		return 1, err
	}
	defer db.Close()
	st, err := db.Load(context.Background())
	if err != nil {
		return 1, err
	}
	if c.id != "" {
		d, ok := st.Deployments[c.id]
		if !ok {
			return 1, fmt.Errorf("deployment %q not found", c.id)
		}
		printJSON(d)
		return model.ExitCode(d), nil
	}
	circuit := safety.CircuitState{State: safety.Closed}
	if st.Circuit != nil {
		circuit = *st.Circuit
	}
	fmt.Printf("circuit: %s %s\n", circuit.State, circuit.Reason)
	for _, d := range st.Deployments {
		fmt.Printf("%-40s %-12s %-10s %-18s %s\n", d.ID, d.Service, d.Phase, d.State, d.Reason)
	}
	return 0, nil
}

func cmdCircuit(ctx context.Context, args []string) (int, error) {
	c := newFlags("circuit")
	reason := c.fs.String("reason", "manual kill switch (CLI)", "reason for trip")
	// Accept both `circuit status -c f` and `circuit -c f status`.
	var action string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	if err := c.fs.Parse(args); err != nil {
		return 1, err
	}
	if action == "" {
		action = c.fs.Arg(0)
	}
	if action == "" {
		return 1, errors.New("circuit: expected status | reset | trip")
	}
	if c.srv != "" {
		var st safety.CircuitState
		var err error
		switch action {
		case "status":
			err = apiCall(ctx, c.srv, http.MethodGet, "/v1/circuit", nil, &st)
		case "reset":
			err = apiCall(ctx, c.srv, http.MethodPost, "/v1/circuit/reset", nil, &st)
		case "trip":
			err = apiCall(ctx, c.srv, http.MethodPost, "/v1/circuit/trip?reason="+strings.ReplaceAll(*reason, " ", "+"), nil, &st)
		default:
			return 1, fmt.Errorf("unknown circuit action %q", action)
		}
		if err != nil {
			return 1, err
		}
		printJSON(st)
		return 0, nil
	}
	e, err := c.engine()
	if err != nil {
		return 1, err
	}
	defer e.Close()
	switch action {
	case "status":
	case "reset":
		e.Breaker.Reset()
		cliAudit(e, c, "circuit.reset", "", "", "")
	case "trip":
		e.Breaker.Trip(*reason)
		cliAudit(e, c, "circuit.trip", "", "", *reason)
	default:
		return 1, fmt.Errorf("unknown circuit action %q", action)
	}
	st := e.Breaker.State()
	printJSON(st)
	if st.State == safety.Open {
		return 3, nil
	}
	return 0, nil
}

func cmdServer(ctx context.Context, args []string) (int, error) {
	c := newFlags("server")
	if err := c.fs.Parse(args); err != nil {
		return 1, err
	}
	e, err := c.engine()
	if err != nil {
		return 1, err
	}
	defer e.Close()
	srv, err := api.New(ctx, e)
	if err != nil {
		return 1, err
	}
	if srv.Auth.Disabled() {
		e.Log.Warn("API authentication disabled: configure auth (service accounts / OIDC) or server.auth_token_env")
	}
	if w := compat.Warning(e.Cfg); w != "" {
		e.Log.Warn(w)
	}
	if sl := e.Cfg.Audit.Syslog; sl != nil {
		x, err := audit.NewExporter(*sl, e.Log)
		if err != nil {
			return 1, err
		}
		e.OnRecord = x.Send
		go x.Run(ctx)
		e.Log.Info("audit records exported", "to", sl.Address, "format", sl.Format)
	}
	role := "single node"
	if e.Cfg.Server.HA.Enabled {
		startHA(ctx, e, srv)
		role = "HA node " + e.Cfg.Server.HA.AdvertiseURL
	} else {
		go func() {
			if n := e.Resume(ctx); n > 0 {
				e.Log.Warn("resumed interrupted rollbacks", "count", n)
			}
		}()
	}
	tc, err := tlsconf.Server(e.Cfg.Server.TLS)
	if err != nil {
		return 1, err
	}
	hs := &http.Server{Addr: e.Cfg.Server.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, TLSConfig: tc}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	e.Log.Info("vigilante server listening", "addr", e.Cfg.Server.Listen, "tls", tc != nil, "role", role, "store", e.Journal.Describe(), "dry_run", e.DryRun, "circuit", e.Breaker.State().State)
	serve := hs.ListenAndServe
	if tc != nil {
		serve = func() error { return hs.ListenAndServeTLS("", "") } // certificates come from TLSConfig
	}
	if err := serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return 1, err
	}
	srv.Close() // let in-flight observations and rollbacks record where they stopped
	return 0, nil
}

func cmdAgent(ctx context.Context, args []string) (int, error) {
	c := newFlags("agent")
	target := c.fs.String("target", orchestrator.Hostname(), "target name of this host in the config")
	if err := c.fs.Parse(args); err != nil {
		return 1, err
	}
	if c.srv == "" {
		return 1, errors.New("agent: --server is required")
	}
	cfg, err := loadConfig(c.config)
	if err != nil {
		return 1, err
	}
	a := &agent.Agent{Cfg: cfg, Target: *target, Server: c.srv, Token: os.Getenv("VIGILANTE_TOKEN"), Log: logger()}
	if err := a.Run(ctx); err != nil {
		return 1, err
	}
	return 0, nil
}

// createGated registers the deployment unless a change freeze refuses new
// ones (exit code 3, like an open circuit). With an override reason it
// proceeds and records who overrode which freeze.
func createGated(e *orchestrator.Engine, c *common, override string) (*model.Deployment, int, error) {
	var ticket *model.ChangeTicket
	if c.id == "" || e.Live(c.id) == nil {
		if err := e.FreezeGate(c.service, override); err != nil {
			return nil, 3, err
		}
		var err error
		if ticket, err = e.ChangeGate(context.Background(), c.service, c.ticket); err != nil {
			return nil, 3, err
		}
	}
	d, err := e.Create(c.id, c.service, c.ver, c.prev)
	if err != nil {
		return nil, 1, err
	}
	e.SetCreatedBy(d, cliActor())
	e.SetChangeTicket(d, ticket)
	if f := e.ActiveFreeze(d.Service, time.Now()); f != nil && override != "" && d.FreezeOverride == "" {
		e.SetFreezeOverride(d, cliActor(), override)
		cliAudit(e, c, "freeze.override", d.Service, d.ID, f.Name+": "+override)
	}
	return d, 0, nil
}
