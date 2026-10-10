package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"vigilante/internal/doctor"
	"vigilante/internal/lab"
	"vigilante/internal/model"
	"vigilante/internal/transport"
)

// cmdLab runs the M8 lab scenario against real equipment and summarizes results:
//
//	vigilante lab run -c FILE --service S --label "F5 BIG-IP VE 17.1" --inject CMD [--reset CMD] [--repeat 3] [--out lab-results.jsonl]
//	vigilante lab summary FILE... [--json]
func cmdLab(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		return 1, errors.New("usage: vigilante lab run|summary ...")
	}
	switch args[0] {
	case "run":
		return labRun(ctx, args[1:])
	case "summary":
		return labSummary(args[1:])
	}
	return 1, fmt.Errorf("unknown lab action %q (run|summary)", args[0])
}

func labRun(ctx context.Context, args []string) (int, error) {
	c := newFlags("lab run")
	label := c.fs.String("label", "", "equipment under test, e.g. \"F5 BIG-IP VE 17.1, vCenter 8.0u2\" (required)")
	inject := c.fs.String("inject", "", "shell command that deploys the bad version (required)")
	reset := c.fs.String("reset", "", "shell command run after every run to restore the good version if the rollback did not")
	phase := c.fs.String("phase", "canary", "phase to observe")
	repeat := c.fs.Int("repeat", 3, "runs")
	baseline := c.fs.Duration("baseline", 0, "capture a baseline for this long before each run (rules comparing with a baseline)")
	out := c.fs.String("out", "lab-results.jsonl", "results file (one JSON line per run, appended)")
	skipDoctor := c.fs.Bool("skip-doctor", false, "do not run doctor first")
	if err := c.fs.Parse(args); err != nil {
		return 1, err
	}
	if c.service == "" || *label == "" || *inject == "" {
		return 1, errors.New("lab run: --service, --label and --inject are required")
	}
	e, err := c.engine()
	if err != nil {
		return 1, err
	}
	defer e.Close()
	if !*skipDoctor {
		mgr := transport.NewManager(e.Cfg)
		checks, err := doctor.Run(ctx, e.Cfg, doctor.Options{Service: c.service, Timeout: 15 * time.Second, Runners: mgr.ForTarget, Log: logger()})
		mgr.Close()
		if err != nil {
			return 1, err
		}
		printChecks(os.Stdout, checks)
		if doctor.Summary(checks)[doctor.Fail] > 0 {
			return 1, errors.New("doctor found failures: fix them first (or --skip-doctor)")
		}
	}
	f, err := os.OpenFile(*out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 1, err
	}
	defer f.Close()
	var results []lab.Result
	for n := 1; n <= *repeat; n++ {
		r := lab.Run(ctx, e, lab.Options{Service: c.service, Phase: model.Phase(*phase), Version: c.ver, Previous: c.prev,
			Inject: *inject, Reset: *reset, Label: *label, Baseline: *baseline}, n)
		line, _ := json.Marshal(r)
		if _, err := f.Write(append(line, '\n')); err != nil {
			return 1, err
		}
		results = append(results, r)
		status := "PASS"
		if !r.Passed {
			status = "FAIL"
		}
		fmt.Printf("run %d/%d %s  final %s  detect %.0fs  rollback %.0fs  (%s)\n", n, *repeat, status, r.FinalState, r.TimeToFail, r.RollbackSeconds, r.DeploymentID)
		for _, s := range r.Steps {
			if !s.OK {
				fmt.Printf("  %s: %s\n", s.Name, s.Detail)
			}
		}
		if ctx.Err() != nil {
			break
		}
	}
	fmt.Print("\n" + lab.Markdown(lab.Summarize(results)))
	fmt.Printf("\nresults appended to %s\n", *out)
	for _, r := range results {
		if !r.Passed {
			return 4, nil
		}
	}
	return 0, nil
}

func labSummary(args []string) (int, error) {
	asJSON := false
	var files []string
	for _, a := range args {
		if a == "--json" {
			asJSON = true
		} else {
			files = append(files, a)
		}
	}
	if len(files) == 0 {
		return 1, errors.New("usage: vigilante lab summary FILE... [--json]")
	}
	var all []lab.Result
	for _, path := range files {
		fh, err := os.Open(path)
		if err != nil {
			return 1, err
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			var r lab.Result
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				fh.Close()
				return 1, fmt.Errorf("%s: %w", path, err)
			}
			all = append(all, r)
		}
		fh.Close()
	}
	s := lab.Summarize(all)
	if asJSON {
		printJSON(s)
	} else {
		fmt.Print(lab.Markdown(s))
	}
	return 0, nil
}
