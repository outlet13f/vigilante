package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/doctor"
	"vigilante/internal/transport"
)

func cmdDoctor(ctx context.Context, args []string) (int, error) {
	c := newFlags("doctor")
	asJSON := c.fs.Bool("json", false, "print results as JSON")
	junit := c.fs.String("junit", "", "also write a JUnit XML report to this file (CI test reports)")
	timeout := c.fs.Duration("timeout", 15*time.Second, "timeout per check")
	if err := c.fs.Parse(args); err != nil {
		return 1, err
	}
	cfg, err := config.Load(c.config)
	if err != nil {
		return 1, err
	}
	mgr := transport.NewManager(cfg)
	defer mgr.Close()
	checks, err := doctor.Run(ctx, cfg, doctor.Options{
		Service: c.service, Version: c.ver, Previous: c.prev, Timeout: *timeout,
		Runners: mgr.ForTarget, Log: logger(),
	})
	if err != nil {
		return 1, err
	}
	if *junit != "" {
		if err := writeJUnit(*junit, checks); err != nil {
			return 1, err
		}
	}
	if *asJSON {
		printJSON(checks)
	} else {
		printChecks(os.Stdout, checks)
	}
	if doctor.Summary(checks)[doctor.Fail] > 0 {
		return 1, doctor.ErrFailed
	}
	return 0, nil
}

var statusLabel = map[doctor.Status]string{doctor.OK: "[OK]  ", doctor.Warn: "[WARN]", doctor.Fail: "[FAIL]", doctor.Skip: "[SKIP]"}

func printChecks(w io.Writer, checks []doctor.Check) {
	for _, ch := range checks {
		fmt.Fprintf(w, "%s %-6s %-16s %s", statusLabel[ch.Status], ch.Scope, ch.Subject, ch.Name)
		if ch.Detail != "" {
			fmt.Fprintf(w, " — %s", ch.Detail)
		}
		fmt.Fprintln(w)
		if ch.Hint != "" && ch.Status != doctor.OK {
			fmt.Fprintf(w, "       → %s\n", ch.Hint)
		}
	}
	s := doctor.Summary(checks)
	fmt.Fprintf(w, "\n점검 %d건: 통과 %d, 경고 %d, 실패 %d, 생략 %d\n", len(checks), s[doctor.OK], s[doctor.Warn], s[doctor.Fail], s[doctor.Skip])
}

type junitSuite struct {
	XMLName  xml.Name    `xml:"testsuite"`
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Skipped  int         `xml:"skipped,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Class   string        `xml:"classname,attr"`
	Name    string        `xml:"name,attr"`
	Failure *junitMessage `xml:"failure,omitempty"`
	Skipped *junitMessage `xml:"skipped,omitempty"`
	Out     string        `xml:"system-out,omitempty"`
}

type junitMessage struct {
	Message string `xml:"message,attr"`
	Body    string `xml:",chardata"`
}

func writeJUnit(path string, checks []doctor.Check) error {
	s := junitSuite{Name: "vigilante-doctor", Tests: len(checks)}
	for _, ch := range checks {
		tc := junitCase{Class: ch.Scope + "." + ch.Subject, Name: ch.Name}
		switch ch.Status {
		case doctor.Fail:
			s.Failures++
			tc.Failure = &junitMessage{Message: ch.Detail, Body: ch.Hint}
		case doctor.Skip:
			s.Skipped++
			tc.Skipped = &junitMessage{Message: ch.Detail}
		case doctor.Warn:
			tc.Out = "WARN: " + ch.Detail + " " + ch.Hint
		}
		s.Cases = append(s.Cases, tc)
	}
	b, err := xml.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append([]byte(xml.Header), b...), 0o644)
}
