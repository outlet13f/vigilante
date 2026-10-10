package orchestrator

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/executor"
	"vigilante/internal/executor/ostest"
	"vigilante/internal/model"
)

// TestOpenStackCanaryRollsBackThroughOctavia runs the M7 scenario end to end
// against the fake OpenStack: snapshot before the deployment, a failing
// canary on a volume-booted VM, drain from Octavia, revert the root volume,
// verify, and put the member back.
func TestOpenStackCanaryRollsBackThroughOctavia(t *testing.T) {
	defer executor.SetPollInterval(time.Millisecond)()
	t.Setenv("VGL_TEST_OS_SECRET", "s3cret-app-cred")
	os := ostest.New(t)
	os.VolumeServer("srv-1")
	os.Members["m1"] = &ostest.Member{ID: "m1", Address: "10.0.0.11", Port: 8080, Up: true, Operating: "ONLINE"}
	os.Members["m2"] = &ostest.Member{ID: "m2", Address: "10.0.0.12", Port: 8080, Up: true, Operating: "ONLINE"}
	app := newFakeApp(true)
	defer app.srv.Close()
	os.OnRevert = func(string) { app.healthy.Store(true) } // the old disk brings the old, healthy release back

	cfg, err := config.Parse([]byte(fmt.Sprintf(`
version: v1
server: {journal_path: %q}
credentials:
  os: {type: openstack, auth_url: %q, region: RegionOne, application_credential_id: ac-1, application_credential_secret_ref: "env:VGL_TEST_OS_SECRET"}
targets:
  - {name: vm-1, address: 10.0.0.11, labels: {url: %q, openstack_server_id: srv-1}, connection: {type: none}}
traffic:
  lb: {type: octavia, octavia: {credential: os, pool_id: pool-1, member_port: 8080}}
executors:
  snap: {type: openstack, openstack: {credential: os}}
services:
  - name: order
    targets: [vm-1]
    probes:
      - {id: health, type: http, interval: 30ms, timeout: 500ms, http: {url: "{{.Labels.url}}"}}
    rules:
      - {name: down, when: {metric: health.consecutive_failures, op: ">=", value: 2}}
    phases:
      canary: {targets: [vm-1], observation_window: 5s, eval_interval: 40ms}
    rollback: {executor: snap, traffic: lb}
`, filepath.ToSlash(filepath.Join(t.TempDir(), "j.jsonl")), os.AuthURL(), app.srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(cfg, Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	ctx := context.Background()

	d, err := e.Create("os-1", "order", "v2", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Prepare(ctx, d); err != nil {
		t.Fatal(err)
	}
	if cp := e.Live("os-1").Checkpoints["vm-1"]; cp["openstack.mode"] != "volume" || cp["openstack.volume_snapshot_id"] == "" {
		t.Fatalf("checkpoint %v", cp)
	}

	app.healthy.Store(false) // v2 is broken
	if err := e.Watch(ctx, d, model.PhaseCanary); err != nil {
		t.Fatal(err)
	}
	got, _ := e.Deployment("os-1")
	if got.State != model.StateRolledBack {
		t.Fatalf("state %s: %s", got.State, got.Reason)
	}
	want := "member 10.0.0.11 up=false,stop,revert,start,member 10.0.0.11 up=true"
	if acts := os.Actions(); acts != want {
		t.Fatalf("actions:\n got  %s\n want %s", acts, want)
	}
	if !os.Members["m2"].Up {
		t.Fatal("a member vigilante does not manage was touched")
	}
	if !strings.Contains(got.Reason, "restored to v1") {
		t.Fatalf("reason: %s", got.Reason)
	}
}
