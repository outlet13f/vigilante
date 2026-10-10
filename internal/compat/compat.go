// Package compat is the compatibility matrix: how far each plugin has been
// verified. A plugin is "verified" only when it ran against the real system
// (equipment, software or cloud) and the versions are recorded; everything
// else is "experimental": covered by automated tests against mocks or
// simulators, not yet by the M8 lab. `vigilante validate` and the server warn
// about experimental plugins in use; docs/09-compatibility.md is the
// published form of this table (a test keeps them in step).
package compat

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"vigilante/internal/config"
)

type Level string

const (
	Verified     Level = "verified"
	Experimental Level = "experimental"
)

type Entry struct {
	Kind  string // probe | executor | traffic
	Type  string
	Level Level
	// Against: what it was verified against (Verified), or how it is tested
	// today (Experimental).
	Against string
}

// Matrix lists every plugin. Update it (and docs/09-compatibility.md) when
// the M8 lab verifies a plugin against real equipment.
var Matrix = []Entry{
	{"probe", "http", Verified, "real HTTP service (end-to-end demo in CI, Linux)"},
	{"probe", "access_log", Verified, "real combined-format access log with rotation (end-to-end demo in CI)"},
	{"probe", "log", Verified, "real application log with rotation (end-to-end demo in CI)"},
	{"probe", "tcp", Experimental, "unit tests against local listeners"},
	{"probe", "grpc", Experimental, "unit tests; grpc.health.v1 against a real server is planned in the M8 lab"},
	{"probe", "host", Experimental, "unit tests on /proc fixtures and a mock SSH runner"},
	{"probe", "docker", Experimental, "unit tests against a mock Docker Engine API"},
	{"probe", "db", Experimental, "unit tests; PostgreSQL and MySQL servers in the M8 lab"},

	{"executor", "webhook", Verified, "real HTTP deploy endpoint with verify URL (end-to-end demo in CI)"},
	{"executor", "exec", Experimental, "unit tests (local and mock SSH runner)"},
	{"executor", "symlink", Experimental, "unit tests on a local release directory and mock systemctl"},
	{"executor", "container", Experimental, "unit tests against a mock Docker Engine API"},
	{"executor", "kvm", Experimental, "unit tests with a mock virsh"},
	{"executor", "vsphere", Experimental, "govmomi vcsim simulator; vCenter 7 and 8 in the M8 lab"},
	{"executor", "nutanix", Experimental, "mock Prism Element v2 API; AOS release paths unconfirmed"},
	{"executor", "openstack", Experimental, "stateful mock of Keystone, Nova, Cinder and Glance; in-house OpenStack in the M8 lab"},

	{"traffic", "nginx", Experimental, "unit tests on generated upstream files with a mock nginx"},
	{"traffic", "haproxy", Experimental, "unit tests against a mock runtime API socket"},
	{"traffic", "envoy", Experimental, "unit tests on generated EDS files"},
	{"traffic", "f5", Experimental, "mock iControl REST; BIG-IP VE in the M8 lab"},
	{"traffic", "aws_alb", Experimental, "mock ELBv2 API; an AWS test account in the M8 lab"},
	{"traffic", "octavia", Experimental, "stateful mock of Octavia v2; in-house OpenStack in the M8 lab"},
}

// Lookup returns the entry for a plugin; unknown plugins (an in-house probe
// registered by a fork) count as experimental.
func Lookup(kind, typ string) Entry {
	for _, e := range Matrix {
		if e.Kind == kind && e.Type == typ {
			return e
		}
	}
	return Entry{Kind: kind, Type: typ, Level: Experimental, Against: "not in the compatibility matrix"}
}

// InUse lists the experimental plugins cfg uses, as "executor vsphere".
func InUse(cfg *config.Config) []string {
	seen := map[string]bool{}
	add := func(kind, typ string) {
		if Lookup(kind, typ).Level != Verified {
			seen[kind+" "+typ] = true
		}
	}
	for _, e := range cfg.Executors {
		add("executor", e.Type)
	}
	for _, t := range cfg.Traffic {
		add("traffic", t.Type)
	}
	for _, s := range cfg.Services {
		for _, p := range s.Probes {
			add("probe", p.Type)
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Warning is the validate/server warning for experimental plugins, or "".
func Warning(cfg *config.Config) string {
	exp := InUse(cfg)
	if len(exp) == 0 {
		return ""
	}
	return fmt.Sprintf("experimental plugins in use (automated tests against mocks/simulators only, not yet verified on real equipment; "+
		"see docs/09-compatibility.md and run `vigilante doctor` against your systems): %s", strings.Join(exp, ", "))
}

// Kinds lists matrix entries of one kind, for `vigilante plugins`.
func Kinds(kind string) []Entry {
	var out []Entry
	for _, e := range Matrix {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Type, b.Type) })
	return out
}
