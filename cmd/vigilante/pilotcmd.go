package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"vigilante/internal/model"
	"vigilante/internal/pilot"
	"vigilante/internal/store"
)

// cmdFeedback records whether a deployment's verdict was right:
//
//	vigilante feedback --id ID --outcome correct|false_positive|false_negative|unclear [--incident INC] [--note TEXT]
func cmdFeedback(ctx context.Context, args []string) (int, error) {
	c := newFlags("feedback")
	outcome := c.fs.String("outcome", "", "correct | false_positive (FAIL on a healthy deployment) | false_negative (harmful deployment not failed) | unclear")
	incident := c.fs.String("incident", "", "incident or problem ticket backing the assessment")
	note := c.fs.String("note", "", "what was found")
	if err := c.fs.Parse(args); err != nil {
		return 1, err
	}
	if c.id == "" || *outcome == "" {
		return 1, errors.New("feedback: --id and --outcome are required")
	}
	body := map[string]string{"outcome": *outcome, "incident": *incident, "note": *note}
	if c.srv != "" {
		var out map[string]any
		if err := apiCall(ctx, c.srv, "PUT", "/v2/deployments/"+url.PathEscape(c.id)+"/feedback", body, &out); err != nil {
			return 1, err
		}
		printJSON(out["feedback"])
		return 0, nil
	}
	e, err := c.engine()
	if err != nil {
		return 1, err
	}
	defer e.Close()
	d, err := e.SetFeedback(c.id, model.Feedback{Outcome: *outcome, Incident: *incident, Note: *note, By: cliActor()}, "cli")
	if err != nil {
		return 1, err
	}
	printJSON(d.Feedback)
	return 0, nil
}

// cmdPilot reports decision quality against the release gate:
//
//	vigilante pilot report -c FILE [--since DATE] [--until DATE] [--service A,B] [--json] [--out FILE]
//
// Exit code 0 when the gate is met, 4 when it is not.
func cmdPilot(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 || args[0] != "report" {
		return 1, errors.New("usage: vigilante pilot report -c FILE [--since DATE] [--until DATE] [--service A,B] [--json] [--out FILE]")
	}
	c := newFlags("pilot")
	since := c.fs.String("since", "", "deployments created from this date (YYYY-MM-DD or RFC 3339)")
	until := c.fs.String("until", "", "deployments created before this date")
	asJSON := c.fs.Bool("json", false, "JSON instead of Markdown")
	out := c.fs.String("out", "", "write the report to this file")
	g := pilot.DefaultGate
	c.fs.IntVar(&g.MinDeployments, "min-deployments", g.MinDeployments, "gate: observed deployments needed")
	c.fs.Float64Var(&g.MaxHoldRate, "max-hold-rate", g.MaxHoldRate, "gate: highest share of held phases")
	if err := c.fs.Parse(args[1:]); err != nil {
		return 1, err
	}
	opt := pilot.Options{Gate: g}
	var err error
	if opt.Since, err = parseDate(*since); err != nil {
		return 1, fmt.Errorf("--since: %w", err)
	}
	if opt.Until, err = parseDate(*until); err != nil {
		return 1, fmt.Errorf("--until: %w", err)
	}
	for _, s := range strings.Split(c.service, ",") { // --service a,b
		if s = strings.TrimSpace(s); s != "" {
			opt.Services = append(opt.Services, s)
		}
	}
	cfg, err := loadConfig(c.config)
	if err != nil {
		return 1, err
	}
	ro := *cfg
	off := false
	ro.Server.State.AutoMigrate = &off // a report never changes the schema
	if be := ro.Server.State.Backend; (be == "" || be == "file") && !exists(ro.Server.JournalPath) {
		return 1, fmt.Errorf("journal %s not found", ro.Server.JournalPath)
	}
	st, err := store.Open(ctx, &ro)
	if err != nil {
		return 1, err
	}
	defer st.Close()
	state, err := st.Load(ctx)
	if err != nil {
		return 1, err
	}
	deps := make([]*model.Deployment, 0, len(state.Deployments))
	for _, d := range state.Deployments {
		deps = append(deps, d)
	}
	r := pilot.Compute(deps, opt)
	var text string
	if *asJSON {
		b, _ := json.MarshalIndent(r, "", "  ")
		text = string(b) + "\n"
	} else {
		text = r.Markdown()
	}
	if *out != "" {
		if err := os.WriteFile(*out, []byte(text), 0o644); err != nil {
			return 1, err
		}
		fmt.Printf("wrote %s (gate %s)\n", *out, map[bool]string{true: "met", false: "not met"}[r.GatePassed])
	} else {
		fmt.Print(text)
	}
	if !r.GatePassed {
		return 4, nil
	}
	return 0, nil
}

func parseDate(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", s)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
