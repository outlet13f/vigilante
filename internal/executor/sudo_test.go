package executor

import (
	"context"
	"strings"
	"testing"

	"vigilante/internal/config"
	"vigilante/internal/transport"
)

// With sudo_scope: changes only the changing commands take sudo, one by one.
func TestSymlinkSudoChangesOnly(t *testing.T) {
	ex, _ := New("s", config.Executor{Type: "symlink", Symlink: &config.SymlinkExec{
		Link: "/opt/app/current", ReleasesDir: "/opt/app/releases", Release: "{{.PreviousVersion}}", Init: "systemd", Unit: "app"}})
	m := &transport.Mock{Sudo: "sudo -n "}
	m.On("readlink -f '/opt/app/current'", "/opt/app/releases/v1\n", nil)
	m.On("readlink -f '/opt/app/releases/v1'", "/opt/app/releases/v1\n", nil)
	m.On("systemctl is-active", "active\n", nil)
	rc := rcFor(m, "v1")
	if err := ex.Rollback(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	if err := ex.Verify(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	cmds := m.Joined()
	for _, want := range []string{"\ntest -d '/opt/app/releases/v1'", "sudo -n ln -sfn '/opt/app/releases/v1' '/opt/app/current.vigilante-tmp'",
		"sudo -n mv -Tf '/opt/app/current.vigilante-tmp' '/opt/app/current'", "sudo -n systemctl restart 'app'"} {
		if !strings.Contains(cmds, want) {
			t.Errorf("missing %q in:\n%s", want, cmds)
		}
	}
	for _, read := range []string{"sudo -n test", "sudo -n readlink", "sudo -n systemctl is-active", "sudo -n sh"} {
		if strings.Contains(cmds, read) {
			t.Errorf("%q must not use sudo:\n%s", read, cmds)
		}
	}
	rules, err := ex.(SudoRules).SudoRules(rc)
	if err != nil || len(rules) != 3 || rules[0].Args != "-sfn /opt/app/releases/* /opt/app/current.vigilante-tmp" ||
		rules[1].Args != "-Tf /opt/app/current.vigilante-tmp /opt/app/current" || rules[2].Command != "systemctl" || rules[0].Host != "app-1" {
		t.Fatalf("rules %+v %v", rules, err)
	}
	// A custom restart_cmd is run as written and needs its own rule.
	ex2, _ := New("s", config.Executor{Type: "symlink", Symlink: &config.SymlinkExec{
		Link: "/l", ReleasesDir: "/r", Release: "{{.PreviousVersion}}", Init: "systemd", Unit: "app", RestartCmd: "/opt/app/bin/restart"}})
	if r2, _ := ex2.(SudoRules).SudoRules(rc); len(r2) != 2 {
		t.Fatalf("custom restart_cmd: %+v", r2)
	}
}

func TestNginxSudoChangesOnly(t *testing.T) {
	conf := "upstream order {\n  server 10.0.0.1:8080;\n  server 10.0.0.2:8080;\n}\n"
	lb := (&transport.Mock{Sudo: "sudo -n "}).On("cat ", conf, nil)
	spec := config.Traffic{Type: "nginx", Nginx: &config.NginxTraffic{
		Hosts: []string{"lb"}, UpstreamFile: "/etc/nginx/up.conf", MemberFormat: "{{.Address}}:8080", TestCmd: "nginx -t", ReloadCmd: "nginx -s reload"}}
	tc, _ := NewTraffic("n", spec, TrafficEnv{Runners: func(string) (transport.Runner, error) { return lb, nil }, Log: testLog})
	if err := tc.Drain(context.Background(), []Member{{Target: "app-1", Data: rcFor(nil, "").Data}}); err != nil {
		t.Fatal(err)
	}
	script := lb.Joined()
	for _, want := range []string{"sudo -n cp -p '/etc/nginx/up.conf' '/etc/nginx/up.conf.vigilante-bak'", "sudo -n tee '/etc/nginx/up.conf.vigilante-new' > /dev/null",
		"sudo -n mv '/etc/nginx/up.conf.vigilante-new' '/etc/nginx/up.conf'", "if ! sudo -n nginx -t", "sudo -n nginx -s reload"} {
		if !strings.Contains(script, want) {
			t.Errorf("missing %q in:\n%s", want, script)
		}
	}
	if strings.Contains(script, "sudo -n cat") {
		t.Error("reading the upstream file must not use sudo")
	}
	rules := tc.(TrafficSudoRules).SudoRules()
	if len(rules) != 6 || rules[1].Command != "tee" || rules[5].Args != "-s reload" || rules[0].Host != "lb" {
		t.Fatalf("rules %+v", rules)
	}
}
