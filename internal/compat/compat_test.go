package compat

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"vigilante/internal/config"
	"vigilante/internal/executor"
	"vigilante/internal/probe"
)

// Every plugin compiled in has a matrix entry, and (full build) the matrix
// has no entry for a plugin that does not exist.
func TestMatrixCoversEveryPlugin(t *testing.T) {
	execs, traffic := executor.Types()
	have := map[string][]string{"probe": probe.Types(), "executor": execs, "traffic": traffic}
	for kind, types := range have {
		for _, typ := range types {
			if Lookup(kind, typ).Against == "not in the compatibility matrix" {
				t.Errorf("%s %s is missing from Matrix", kind, typ)
			}
		}
	}
	for _, e := range Matrix {
		if fullBuild && !slices.Contains(have[e.Kind], e.Type) {
			t.Errorf("Matrix lists %s %s, which is not registered", e.Kind, e.Type)
		}
		if e.Level == Verified && e.Against == "" {
			t.Errorf("%s %s: verified needs what it was verified against", e.Kind, e.Type)
		}
	}
}

// docs/09-compatibility.md publishes the same levels.
func TestDocMatchesMatrix(t *testing.T) {
	b, err := os.ReadFile("../../docs/09-compatibility.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("(?m)^\\| `([a-z_0-9]+)` \\| (검증됨|실험적) \\|")
	doc := map[string]Level{}
	for _, m := range row.FindAllStringSubmatch(string(b), -1) {
		lv := Experimental
		if m[2] == "검증됨" {
			lv = Verified
		}
		doc[m[1]] = lv
	}
	for _, e := range Matrix {
		got, ok := doc[e.Type]
		if !ok {
			t.Errorf("docs/09-compatibility.md has no row for %s %s", e.Kind, e.Type)
		} else if got != e.Level {
			t.Errorf("%s %s: doc says %s, Matrix says %s", e.Kind, e.Type, got, e.Level)
		}
	}
	if len(doc) != len(Matrix) {
		t.Errorf("doc has %d plugin rows, Matrix %d", len(doc), len(Matrix))
	}
}

func TestWarningListsExperimentalPluginsInUse(t *testing.T) {
	cfg := &config.Config{
		Executors: map[string]config.Executor{"a": {Type: "webhook"}, "b": {Type: "vsphere"}},
		Traffic:   map[string]config.Traffic{"lb": {Type: "f5"}},
		Services:  []config.Service{{Probes: []config.Probe{{Type: "http"}, {Type: "grpc"}}}},
	}
	w := Warning(cfg)
	for _, want := range []string{"executor vsphere", "traffic f5", "probe grpc"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning misses %s: %s", want, w)
		}
	}
	for _, verified := range []string{"webhook", "probe http"} {
		if strings.Contains(w, verified) {
			t.Errorf("warning lists verified plugin %s: %s", verified, w)
		}
	}
	if Warning(&config.Config{Executors: map[string]config.Executor{"a": {Type: "webhook"}}}) != "" {
		t.Error("only verified plugins: no warning expected")
	}
}
