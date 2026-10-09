package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const presetBase = `
version: v1
targets: [{name: a, address: 10.0.0.1}, {name: b, address: 10.0.0.2}]
executors: {x: {type: exec, exec: {rollback: "true"}}}
services:
  - name: s
    targets: [a, b]
    rollback: {executor: x}
`

// Every built-in preset, filled with its required params only, must produce a
// service that passes full validation.
func TestBuiltinPresetsProduceValidServices(t *testing.T) {
	required := map[string]string{
		"java-web":      `{health_url: "http://{{.Address}}:8080/health", access_log: /var/log/a.log, app_log: /var/log/app.log}`,
		"container-api": `{container: api, health_url: "http://{{.Address}}:8080/healthz"}`,
		"static-web":    `{health_url: "http://{{.Address}}/healthz", access_log: /var/log/nginx/access.log}`,
		"worker":        `{app_log: /var/log/worker.log}`,
	}
	for name, ov := range required {
		c, err := Parse([]byte(presetBase + "    preset: " + name + "\n    overrides: " + ov + "\n"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		s := c.Services[0]
		if s.Preset != name+"@1" || len(s.Probes) == 0 || len(s.Rules) == 0 || len(s.Phases) == 0 {
			t.Fatalf("%s expanded to %+v", name, s)
		}
		if s.Probes[0].HTTP != nil && !strings.Contains(s.Probes[0].HTTP.URL, "{{.Address}}") {
			t.Fatalf("%s: runtime template was consumed: %q", name, s.Probes[0].HTTP.URL)
		}
	}
}

func TestPresetOverridesAndOptionalSections(t *testing.T) {
	c, err := Parse([]byte(presetBase + `    preset: container-api@1
    overrides: {container: api, health_url: "http://x/h", access_log: /var/log/api.log, error_rate_pct: 5}
`))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Services[0]
	if _, ok := s.Probe("access"); !ok {
		t.Fatal("access_log override must add the access probe")
	}
	var found bool
	for _, r := range s.Rules {
		for _, n := range r.When.Any {
			if n.Metric == "access.count_5xx" && n.Value != nil && *n.Value == 5 {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("error_rate_pct override not applied")
	}
	// Without access_log the access probe and its rule clause are left out.
	c, err = Parse([]byte(presetBase + "    preset: container-api\n    overrides: {container: api, health_url: \"http://x/h\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Services[0].Probe("access"); ok {
		t.Fatal("access probe present without access_log")
	}
}

func TestServiceDefinitionsWinOverPreset(t *testing.T) {
	c, err := Parse([]byte(presetBase + `    preset: static-web
    overrides: {health_url: "http://x/h", access_log: /a.log}
    probes:
      - {id: health, type: tcp, tcp: {address: "x:80"}}
    rules:
      - {name: errors, when: {metric: health.consecutive_failures, op: ">=", value: 9}}
    phases:
      canary: {targets: [b]}
`))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Services[0]
	if p, _ := s.Probe("health"); p.Type != "tcp" {
		t.Fatal("service probe must replace the preset probe with the same id")
	}
	for _, r := range s.Rules {
		if r.Name == "errors" && *r.When.Value != 9 {
			t.Fatal("service rule must replace the preset rule with the same name")
		}
	}
	pc := s.Phases["canary"]
	if len(pc.Targets) != 1 || pc.Targets[0] != "b" || pc.ObservationWindow != 5*time.Minute {
		t.Fatalf("phase merge: %+v", pc)
	}
}

func TestPresetErrors(t *testing.T) {
	cases := map[string]string{
		"    preset: java-web\n    overrides: {health_url: x}\n":                                 "required override missing: access_log, app_log",
		"    preset: static-web\n    overrides: {health_url: x, access_log: y, error_rate: 3}\n": "unknown override [error_rate]",
		"    preset: nope\n": `unknown preset "nope"`,
		"    preset: java-web@9\n    overrides: {health_url: x, access_log: y, app_log: z}\n": "java-web@9 not found",
		"    overrides: {a: 1}\n": "overrides given without a preset",
	}
	for tail, want := range cases {
		_, err := Parse([]byte(presetBase + tail))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", strings.TrimSpace(tail), err, want)
		}
	}
}

func TestOrganisationPresetDir(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "presets"), 0o755)
	os.WriteFile(filepath.Join(dir, "presets", "corp-api.yaml"), []byte(`name: corp/api
version: 2
description: house standard
params:
  - {name: port, default: 8080}
---
probes:
  - {id: health, type: http, http: {url: "http://{{.Address}}:[[ .port ]]/ping"}}
rules:
  - {name: down, when: {metric: health.consecutive_failures, op: ">=", value: 3}}
`), 0o644)
	cfg := strings.Replace(presetBase, "version: v1\n", "version: v1\npreset_dirs: [presets]\n", 1) + "    preset: corp/api@2\n    overrides: {port: 9000}\n"
	path := filepath.Join(dir, "vigilante.yaml")
	os.WriteFile(path, []byte(cfg), 0o644)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := c.Services[0].Probe("health"); p.HTTP.URL != "http://{{.Address}}:9000/ping" {
		t.Fatalf("url %q", p.HTTP.URL)
	}
}
