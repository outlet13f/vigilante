package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"vigilante/internal/audit"
	"vigilante/internal/journal"
	"vigilante/internal/orchestrator"
	"vigilante/internal/store"
)

// cliAudit records a local CLI action in the journal.
func cliAudit(e *orchestrator.Engine, c *common, action, service, deployID, reason string) {
	e.Audit(journal.Entry{Actor: cliActor(), Source: "cli", Action: action, Service: service, DeployID: deployID,
		Reason: reason, Ticket: c.ticket})
}

// cmdAudit: verify | export | prune | query
func cmdAudit(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return 1, errors.New("usage: vigilante audit verify|export|prune|query -c FILE ...")
	}
	action := args[0]
	c := newFlags("audit")
	file := c.fs.String("file", "", "verify: check this archive file instead of the configured store")
	out := c.fs.String("out", "", "export/prune: output file (prune: the archive)")
	before := c.fs.String("before", "", "prune: remove entries older than this date (YYYY-MM-DD)")
	olderThan := c.fs.Duration("older-than", 0, "prune: remove entries older than this age (default audit.retention)")
	actor := c.fs.String("actor", "", "query: filter by actor")
	action2 := c.fs.String("action", "", "query: filter by action (e.g. denied, rollback.manual)")
	since := c.fs.String("since", "", "query: from date/time (YYYY-MM-DD or RFC 3339)")
	limit := c.fs.Int("limit", 200, "query: max records")
	if err := c.fs.Parse(args[1:]); err != nil {
		return 1, err
	}
	if action == "verify" && *file != "" {
		r, err := audit.VerifyFile(*file)
		return reportVerify(r, err, *file)
	}
	cfg, err := loadConfig(c.config)
	if err != nil {
		return 1, err
	}
	st, err := store.Open(ctx, cfg)
	if err != nil {
		return 1, err
	}
	defer st.Close()

	switch action {
	case "verify":
		r, err := audit.Verify(ctx, st)
		return reportVerify(r, err, st.Describe())
	case "export":
		if *out == "" {
			return 1, errors.New("audit export: --out FILE required")
		}
		f, err := os.Create(*out)
		if err != nil {
			return 1, err
		}
		n, err := audit.Export(ctx, st, f)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return 1, err
		}
		fmt.Printf("exported %d entries to %s (verify with: vigilante audit verify --file %s)\n", n, *out, *out)
		return 0, nil
	case "prune":
		cut, err := pruneCutoff(*before, *olderThan, cfg.Audit.Retention)
		if err != nil {
			return 1, err
		}
		if *out == "" {
			return 1, errors.New("audit prune: --out ARCHIVE.jsonl required (pruned entries are archived first)")
		}
		f, err := os.OpenFile(*out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return 1, fmt.Errorf("archive %s: %w (choose a new file)", *out, err)
		}
		n, err := st.Prune(ctx, cut, f)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return 1, err
		}
		if n == 0 {
			os.Remove(*out)
			fmt.Printf("nothing older than %s\n", cut.Format("2006-01-02"))
			return 0, nil
		}
		fmt.Printf("pruned %d entries older than %s into %s; the journal continues from an anchor\n", n, cut.Format("2006-01-02"), *out)
		return 0, nil
	case "query":
		f := audit.Filter{Actor: *actor, Action: *action2, Service: c.service, Limit: *limit}
		if *since != "" {
			t, err := parseWhen(*since)
			if err != nil {
				return 1, err
			}
			f.Since = t
		}
		entries, err := audit.Query(ctx, st, f)
		if err != nil {
			return 1, err
		}
		for _, e := range entries {
			subject := e.DeployID
			if e.Deployment != nil {
				subject = e.Deployment.ID + " " + string(e.Deployment.State)
			}
			if e.Circuit != nil {
				subject = "circuit " + string(e.Circuit.State)
			}
			what := e.Action
			if what == "" {
				what = e.Kind
			}
			fmt.Printf("%s  %-28s %-22s %-24s %s\n", e.Time.Local().Format("2006-01-02 15:04:05"), e.Actor, what, subject, e.Reason)
		}
		return 0, nil
	}
	return 1, fmt.Errorf("audit: unknown action %q", action)
}

func reportVerify(r audit.Report, err error, what string) (int, error) {
	if err != nil {
		return 1, err
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	fmt.Println(string(b))
	if !r.OK {
		return 1, fmt.Errorf("audit chain of %s is broken: %s", what, r.Broken)
	}
	fmt.Fprintf(os.Stderr, "audit chain of %s intact: %d entries verified", what, r.Checked)
	if r.Legacy > 0 {
		fmt.Fprintf(os.Stderr, " (%d older entries predate the chain)", r.Legacy)
	}
	fmt.Fprintln(os.Stderr)
	return 0, nil
}

func pruneCutoff(before string, olderThan, retention time.Duration) (time.Time, error) {
	switch {
	case before != "":
		return time.Parse("2006-01-02", before)
	case olderThan > 0:
		return time.Now().Add(-olderThan), nil
	case retention > 0:
		return time.Now().Add(-retention), nil
	}
	return time.Time{}, errors.New("audit prune: give --before, --older-than, or set audit.retention")
}

func parseWhen(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.ParseInLocation("2006-01-02", s, time.Local)
}
