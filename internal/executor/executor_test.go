package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"vigilante/internal/config"
	"vigilante/internal/tmpl"
	"vigilante/internal/transport"
)

var testLog = slog.New(slog.NewTextHandler(io.Discard, nil))

func rcFor(r transport.Runner, prev string) *RunContext {
	t := config.Target{Name: "app-1", Address: "10.0.0.1", Labels: map[string]string{"instance_id": "i-1"}}
	d := tmpl.ForTarget(t)
	d.Service, d.Version, d.PreviousVersion, d.DeploymentID = "order", "v2", prev, "dep-1"
	return &RunContext{Target: t, Data: d, Runner: r, Checkpoint: map[string]string{}, Log: testLog,
		Runners: func(string) (transport.Runner, error) { return r, nil }}
}

func TestSymlinkRollbackAndVerify(t *testing.T) {
	ex, _ := New("s", config.Executor{Type: "symlink", Symlink: &config.SymlinkExec{
		Link: "/opt/app/current", ReleasesDir: "/opt/app/releases", Release: "{{.PreviousVersion}}", Init: "systemd", Unit: "app"}})
	m := &transport.Mock{}
	m.On("readlink -f '/opt/app/current'", "/opt/app/releases/v1\n", nil)
	m.On("readlink -f '/opt/app/releases/v1'", "/opt/app/releases/v1\n", nil)
	m.On("systemctl is-active", "active\n", nil)
	rc := rcFor(m, "v1")
	if err := ex.Rollback(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	cmds := m.Joined()
	for _, want := range []string{"test -d '/opt/app/releases/v1'", "ln -sfn '/opt/app/releases/v1' '/opt/app/current.vigilante-tmp'", "mv -Tf '/opt/app/current.vigilante-tmp' '/opt/app/current'", "systemctl restart 'app'"} {
		if !strings.Contains(cmds, want) {
			t.Errorf("missing %q in:\n%s", want, cmds)
		}
	}
	if err := ex.Verify(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	// Checkpoint from prepare overrides the computed release path.
	rc.Checkpoint["symlink.previous"] = "/opt/app/releases/v0-hotfix"
	m2 := &transport.Mock{}
	rc.Runner = m2
	_ = ex.Rollback(context.Background(), rc)
	if !strings.Contains(m2.Joined(), "v0-hotfix") {
		t.Fatal("checkpoint not used")
	}
}

func TestSymlinkFailurePropagates(t *testing.T) {
	ex, _ := New("s", config.Executor{Type: "symlink", Symlink: &config.SymlinkExec{
		Link: "/l", ReleasesDir: "/r", Release: "{{.PreviousVersion}}", Init: "none"}})
	m := (&transport.Mock{}).On("set -e", "", errors.New("exit 1: no such dir"))
	if err := ex.Rollback(context.Background(), rcFor(m, "v1")); err == nil {
		t.Fatal("expected error")
	}
}

func TestDryRunDoesNotMutate(t *testing.T) {
	ex, _ := New("s", config.Executor{Type: "symlink", Symlink: &config.SymlinkExec{
		Link: "/l", ReleasesDir: "/r", Release: "{{.PreviousVersion}}", Init: "systemd", Unit: "u"}})
	m := &transport.Mock{}
	rc := rcFor(m, "v1")
	rc.DryRun = true
	if err := ex.Rollback(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	if len(m.Commands) != 0 {
		t.Fatalf("dry-run executed: %v", m.Commands)
	}
}

func TestNginxDownRewrite(t *testing.T) {
	conf := "upstream order {\n  server 10.0.0.1:8080 max_fails=3;\n  server 10.0.0.2:8080 down;\n  server 10.0.0.3:8080;\n}\n"
	out, missing := SetNginxDown(conf, map[string]bool{"10.0.0.1:8080": true, "10.0.0.9:8080": true}, true)
	if !strings.Contains(out, "server 10.0.0.1:8080 max_fails=3 down;") || len(missing) != 1 {
		t.Fatalf("drain:\n%s missing=%v", out, missing)
	}
	out, _ = SetNginxDown(out, map[string]bool{"10.0.0.1:8080": true, "10.0.0.2:8080": true}, false)
	if strings.Contains(out, "down") {
		t.Fatalf("enable left down flags:\n%s", out)
	}
}

func TestNginxController(t *testing.T) {
	conf := "upstream order {\n  server 10.0.0.1:8080;\n  server 10.0.0.2:8080;\n}\n"
	lb := (&transport.Mock{}).On("cat ", conf, nil)
	tc, _ := NewTraffic("n", config.Traffic{Type: "nginx", Nginx: &config.NginxTraffic{
		Hosts: []string{"lb"}, UpstreamFile: "/etc/nginx/up.conf", MemberFormat: "{{.Address}}:8080", TestCmd: "nginx -t", ReloadCmd: "nginx -s reload"}},
		TrafficEnv{Runners: func(string) (transport.Runner, error) { return lb, nil }, Log: testLog})
	pool, _ := tc.Pool(context.Background())
	if len(pool) != 2 || !pool[0].Enabled {
		t.Fatalf("pool %+v", pool)
	}
	if err := tc.Drain(context.Background(), []Member{{Target: "app-1", Data: rcFor(nil, "").Data}}); err != nil {
		t.Fatal(err)
	}
	if len(lb.Stdins) != 1 || !strings.Contains(lb.Stdins[0], "server 10.0.0.1:8080 down;") {
		t.Fatalf("written config: %v", lb.Stdins)
	}
	if !strings.Contains(lb.Joined(), "if ! nginx -t") || !strings.Contains(lb.Joined(), "nginx -s reload") {
		t.Fatalf("script lacks test/reload guard:\n%s", lb.Joined())
	}
}

// fakeHAProxy answers runtime API commands over an in-memory pipe.
func fakeHAProxy(t *testing.T, seen *[]string, mu *sync.Mutex) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network != "unix" || addr != "/run/haproxy/admin.sock" {
			t.Errorf("dial %s %s", network, addr)
		}
		c, s := net.Pipe()
		go func() {
			defer s.Close()
			line, _ := bufio.NewReader(s).ReadString('\n')
			mu.Lock()
			*seen = append(*seen, strings.TrimSpace(line))
			mu.Unlock()
			if strings.HasPrefix(line, "show servers state") {
				io.WriteString(s, "1\n# be_id be_name srv_id srv_name srv_addr srv_op_state srv_admin_state\n3 pay 1 app-1 10.0.0.1 2 0\n3 pay 2 app-2 10.0.0.2 2 1\n")
			}
		}()
		return c, nil
	}
}

func TestHAProxyRuntimeAPI(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	lb := &transport.Mock{DialFunc: fakeHAProxy(t, &seen, &mu)}
	tc, _ := NewTraffic("h", config.Traffic{Type: "haproxy", HAProxy: &config.HAProxyTraffic{
		Hosts: []string{"lb"}, Socket: "/run/haproxy/admin.sock", Backend: "pay", ServerName: "{{.Name}}"}},
		TrafficEnv{Runners: func(string) (transport.Runner, error) { return lb, nil }, Log: testLog})
	pool, err := tc.Pool(context.Background())
	if err != nil || len(pool) != 2 || !pool[0].Enabled || pool[1].Enabled {
		t.Fatalf("pool %+v %v", pool, err)
	}
	m := []Member{{Target: "app-1", Data: rcFor(nil, "").Data}}
	if err := tc.Drain(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if err := tc.Enable(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	got := strings.Join(seen, "|")
	if !strings.Contains(got, "set server pay/app-1 state drain") || !strings.Contains(got, "set server pay/app-1 state ready") {
		t.Fatalf("commands: %s", got)
	}
}

func TestEnvoyEDS(t *testing.T) {
	eds := `{"version_info":"1","resources":[{"@type":"type.googleapis.com/envoy.config.endpoint.v3.ClusterLoadAssignment","cluster_name":"order","endpoints":[{"lb_endpoints":[
	 {"endpoint":{"address":{"socket_address":{"address":"10.0.0.1","port_value":8080}}}},
	 {"endpoint":{"address":{"socket_address":{"address":"10.0.0.2","port_value":8080}}}}]}]}]}`
	lb := (&transport.Mock{}).On("cat ", eds, nil)
	tc, _ := NewTraffic("e", config.Traffic{Type: "envoy", Envoy: &config.EnvoyTraffic{Hosts: []string{"lb"}, EDSFile: "/etc/envoy/eds.json", MemberFormat: "{{.Address}}:8080"}},
		TrafficEnv{Runners: func(string) (transport.Runner, error) { return lb, nil }, Log: testLog})
	if err := tc.Drain(context.Background(), []Member{{Target: "app-1", Data: rcFor(nil, "").Data}}); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(lb.Stdins[0]), &doc); err != nil {
		t.Fatal(err)
	}
	drained := 0
	walkEnvoyEndpoints(doc, func(id string, ep map[string]any) {
		if ep["health_status"] == "DRAINING" {
			if id != "10.0.0.1:8080" {
				t.Errorf("wrong endpoint drained: %s", id)
			}
			drained++
		}
	})
	if drained != 1 || !strings.Contains(lb.Joined(), "mv '/etc/envoy/eds.json.vigilante-new' '/etc/envoy/eds.json'") {
		t.Fatalf("drained=%d cmds=%s", drained, lb.Joined())
	}
}

func TestF5iControl(t *testing.T) {
	var mu sync.Mutex
	var patches []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, _ := r.BasicAuth(); u != "admin" || p != "secret" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/mgmt/tm/ltm/pool/~Common~order_pool/members":
			io.WriteString(w, `{"items":[{"name":"10.0.0.1:8080","session":"monitor-enabled","state":"up"},{"name":"10.0.0.2:8080","session":"user-disabled","state":"up"}]}`)
		case r.Method == http.MethodPatch:
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			patches = append(patches, r.URL.Path+" "+string(b))
			mu.Unlock()
			io.WriteString(w, `{}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	t.Setenv("F5U", "admin")
	t.Setenv("F5P", "secret")
	tc, _ := NewTraffic("f5", config.Traffic{Type: "f5", F5: &config.F5Traffic{URL: srv.URL, Credential: "f5", Pool: "/Common/order_pool", MemberFormat: "{{.Address}}:8080", ForceOffline: true}},
		TrafficEnv{Creds: map[string]config.Credential{"f5": {Type: "basic", UsernameEnv: "F5U", PasswordEnv: "F5P"}}, Log: testLog, HTTP: srv.Client()})
	pool, err := tc.Pool(context.Background())
	if err != nil || len(pool) != 2 || !pool[0].Enabled || pool[1].Enabled {
		t.Fatalf("pool %+v %v", pool, err)
	}
	m := []Member{{Target: "app-1", Data: rcFor(nil, "").Data}}
	if err := tc.Drain(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if err := tc.Enable(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	want := "/mgmt/tm/ltm/pool/~Common~order_pool/members/~Common~10.0.0.1:8080"
	if len(patches) != 2 || !strings.HasPrefix(patches[0], want) || !strings.Contains(patches[0], `"state":"user-down"`) || !strings.Contains(patches[1], `"session":"user-enabled"`) {
		t.Fatalf("patches %v", patches)
	}
}

func TestKVMSnapshotRevert(t *testing.T) {
	ex, _ := New("k", config.Executor{Type: "kvm", KVM: &config.KVMExec{Hypervisor: "kvm-1", Domain: "{{.Name}}", Snapshot: "vigilante-{{.Version}}"}})
	m := (&transport.Mock{}).On("virsh domstate", "running\n", nil)
	rc := rcFor(m, "v1")
	cp, err := ex.(Preparer).Prepare(context.Background(), rc)
	if err != nil || cp["kvm.snapshot"] != "vigilante-v2" {
		t.Fatalf("prepare %v %v", cp, err)
	}
	if err := ex.Rollback(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	if err := ex.Verify(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.Joined(), "virsh snapshot-revert --domain 'app-1' --snapshotname 'vigilante-v2' --running") {
		t.Fatal(m.Joined())
	}
}

func TestWebhookExecutor(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = r.Method + " " + r.URL.Path + " " + r.Header.Get("Authorization") + " " + string(b)
	}))
	defer srv.Close()
	t.Setenv("TOK", "abc")
	ex, _ := New("w", config.Executor{Type: "webhook", Webhook: &config.WebhookExec{
		URL: srv.URL + "/rollback/{{.Name}}", Method: "POST", Credential: "c", Body: `{"to":"{{.PreviousVersion}}"}`}})
	rc := rcFor(nil, "v1")
	rc.Creds = map[string]config.Credential{"c": {Type: "token", TokenEnv: "TOK"}}
	if err := ex.Rollback(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	if got != `POST /rollback/app-1 Bearer abc {"to":"v1"}` {
		t.Fatalf("got %q", got)
	}
}
