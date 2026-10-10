package main

import (
	"database/sql"
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"

	"vigilante/internal/config"
	"vigilante/internal/executor"
	"vigilante/internal/probe"
)

// Release builds set these with
// -ldflags "-X main.version=1.2.3 -X main.commit=<sha> -X main.date=<RFC 3339>".
var (
	commit = ""
	date   = ""
)

// buildInfo is the `vigilante version` line.
func buildInfo() string {
	c := commit
	if c == "" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" && len(s.Value) >= 12 {
					c = s.Value[:12]
				}
			}
		}
	}
	parts := []string{flavor + " build"}
	if c != "" {
		parts = append(parts, "commit "+c)
	}
	if date != "" {
		parts = append(parts, "built "+date)
	}
	parts = append(parts, runtime.Version()+" "+runtime.GOOS+"/"+runtime.GOARCH)
	return fmt.Sprintf("vigilante %s (%s)", version, strings.Join(parts, ", "))
}

// checkBuild reports plugins the config uses that this binary does not
// contain (the minimal build leaves out vSphere, AWS ALB, gRPC and MySQL).
func checkBuild(cfg *config.Config) error {
	execs, traffic := executor.Types()
	probes := probe.Types()
	var missing []string
	for name, e := range cfg.Executors {
		if !slices.Contains(execs, e.Type) {
			missing = append(missing, fmt.Sprintf("executors.%s: type %s", name, e.Type))
		}
	}
	for name, t := range cfg.Traffic {
		if !slices.Contains(traffic, t.Type) {
			missing = append(missing, fmt.Sprintf("traffic.%s: type %s", name, t.Type))
		}
	}
	for _, s := range cfg.Services {
		for _, p := range s.Probes {
			if !slices.Contains(probes, p.Type) {
				missing = append(missing, fmt.Sprintf("service %s probe %s: type %s", s.Name, p.ID, p.Type))
			} else if p.DB != nil && p.DB.Driver == "mysql" && !slices.Contains(sql.Drivers(), "mysql") {
				missing = append(missing, fmt.Sprintf("service %s probe %s: driver mysql", s.Name, p.ID))
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	slices.Sort(missing)
	return errors.New("this is the " + flavor + " build of vigilante, which does not include: " +
		strings.Join(missing, "; ") + ". Install the full build (vigilante_<version>_<os>_<arch>, without -minimal)")
}
