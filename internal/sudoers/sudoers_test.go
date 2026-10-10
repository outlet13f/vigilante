package sudoers

import (
	"context"
	"strings"
	"testing"

	"vigilante/internal/config"
	"vigilante/internal/transport"
)

const cfgYAML = `
version: v1
credentials:
  key: {type: ssh, user: deploy, private_key_file: /k}
targets:
  - {name: app-1, address: 10.0.0.1, connection: {type: ssh, credential: key, sudo: true, sudo_scope: changes}}
  - {name: kvm-1, address: 10.0.0.5, connection: {type: ssh, credential: key, sudo: true, sudo_scope: changes}}
  - {name: lb-1, address: 10.0.0.9, connection: {type: ssh, credential: key}}
traffic:
  edge: {type: nginx, nginx: {hosts: [lb-1], upstream_file: "/etc/nginx/conf.d/order=up.conf", member_format: "{{.Address}}:8080"}}
executors:
  link: {type: symlink, symlink: {link: /opt/order/current, releases_dir: /opt/order/releases, init: systemd, unit: order}}
  snap: {type: kvm, kvm: {hypervisor: kvm-1, domain: "order-{{.Name}}", snapshot: "pre-{{.Version}}"}}
  hook: {type: exec, exec: {rollback: "/opt/order/bin/rollback", on: target}}
services:
  - name: order
    targets: [app-1]
    probes: [{id: h, type: tcp, tcp: {address: "x:1"}}]
    rules: [{name: down, when: {metric: h.up, op: "==", value: 0}}]
    rollback:
      executor: link
      traffic: edge
      mode: auto
      escalation: [{executor: snap}, {executor: hook}]
`

func TestRulesAndRender(t *testing.T) {
	cfg, err := config.Parse([]byte(cfgYAML))
	if err != nil {
		t.Fatal(err)
	}
	rules, notes := Rules(cfg)
	hosts := Group(cfg, rules)
	by := map[string]*Host{}
	for _, h := range hosts {
		by[h.Name] = h
	}
	if len(by["app-1"].Rules) != 3 || len(by["kvm-1"].Rules) != 2 || len(by["lb-1"].Rules) != 6 {
		t.Fatalf("rules per host: app %d kvm %d lb %d", len(by["app-1"].Rules), len(by["kvm-1"].Rules), len(by["lb-1"].Rules))
	}
	if by["kvm-1"].Rules[0].Args != "snapshot-create-as --domain order-app-1 --name * --description vigilante pre-deploy checkpoint --atomic" {
		t.Fatalf("kvm rule: %+v", by["kvm-1"].Rules[0])
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "hook (exec)") {
		t.Fatalf("notes %v", notes)
	}
	// Paths from the host when it answers, defaults otherwise.
	m := (&transport.Mock{}).On("command -v 'systemctl'", "/bin/systemctl\n", nil).On("command -v", "", nil)
	h := by["app-1"]
	for _, r := range h.Rules {
		p, ok := Resolve(context.Background(), m, r.Command)
		h.Paths[r.Command], h.Guessed[r.Command] = p, !ok
	}
	out := h.Render()
	for _, want := range []string{
		"deploy ALL=(root) NOPASSWD: /usr/bin/ln -sfn /opt/order/releases/* /opt/order/current.vigilante-tmp",
		"deploy ALL=(root) NOPASSWD: /usr/bin/mv -Tf /opt/order/current.vigilante-tmp /opt/order/current",
		"deploy ALL=(root) NOPASSWD: /bin/systemctl restart order",
		"Defaults:deploy !requiretty", "(path not confirmed on the host)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// lb-1 has no sudo yet: the file says so; '=' in a path is escaped for sudoers.
	lb := by["lb-1"].Render()
	if !strings.Contains(lb, "does not have connection.sudo: true") || !strings.Contains(lb, `/etc/nginx/conf.d/order\=up.conf.vigilante-new`) {
		t.Errorf("lb rules:\n%s", lb)
	}
}
