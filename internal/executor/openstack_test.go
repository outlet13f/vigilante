package executor

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/executor/ostest"
	"vigilante/internal/tmpl"
)

// ---------------------------------------------------------------- helpers

func osCreds(f *ostest.Fake) map[string]config.Credential {
	return map[string]config.Credential{"os": {Type: "openstack", AuthURL: f.Srv.URL + "/identity/v3", Region: "RegionOne",
		ApplicationCredentialID: "ac-1", ApplicationCredentialSecretRef: "env:VGL_TEST_OS_SECRET"}}
}

func osRC(t *testing.T, f *ostest.Fake, server string, cp map[string]string) *RunContext {
	t.Setenv("VGL_TEST_OS_SECRET", "s3cret-app-cred")
	d := tmpl.ForTarget(config.Target{Name: "app-1", Address: "10.0.0.11", Labels: map[string]string{"openstack_server_id": server}})
	d.Service, d.Version, d.DeploymentID = "order", "v2", "dep-1"
	if cp == nil {
		cp = map[string]string{}
	}
	return &RunContext{Target: config.Target{Name: "app-1"}, Data: d, Creds: osCreds(f), Checkpoint: cp,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func osExec(t *testing.T, mode string) *openstackExec {
	t.Helper()
	pollInterval = time.Millisecond
	t.Cleanup(func() { pollInterval = 3 * time.Second })
	cfg, err := config.Parse([]byte(`
version: v1
credentials: {os: {type: openstack, auth_url: "http://x/v3", application_credential_id: ac-1, application_credential_secret_ref: "env:X"}}
targets: [{name: a}]
executors: {vm: {type: openstack, openstack: {credential: os, mode: ` + mode + `}}}
services:
  - name: s
    targets: [a]
    probes: [{id: h, type: tcp, tcp: {address: "x:1"}}]
    rules: [{name: down, when: {metric: h.Up, op: "==", value: 0}}]
    rollback: {executor: vm}
`))
	if err != nil {
		t.Fatal(err)
	}
	ex, err := New("vm", cfg.Executors["vm"])
	if err != nil {
		t.Fatal(err)
	}
	return ex.(*openstackExec)
}

// ---------------------------------------------------------------- executor

func TestOpenStackVolumeSnapshotRevertAndReplay(t *testing.T) {
	f := ostest.New(t)
	f.VolumeServer("srv-1")
	o := osExec(t, "auto")
	ctx := context.Background()

	// A snapshot not made by vigilante is never pruned.
	f.Snapshots["manual"] = &ostest.Snapshot{Volume: "vol-srv-1", Status: "available", Created: "2000-01-01T00:00:00Z", Meta: map[string]string{}}
	f.Volumes["vol-srv-1"].Snaps = append(f.Volumes["vol-srv-1"].Snaps, "manual")

	var cp map[string]string
	for i := 0; i < 5; i++ { // five deployments: only the newest 3 snapshots are kept
		var err error
		if cp, err = o.Prepare(ctx, osRC(t, f, "srv-1", nil)); err != nil {
			t.Fatal(err)
		}
	}
	if cp[cpMode] != "volume" || cp[cpVolume] != "vol-srv-1" || cp[cpSnapshot] == "" || cp[cpServer] != "srv-1" {
		t.Fatalf("checkpoint %v", cp)
	}
	live := 0
	for id, s := range f.Snapshots {
		if s.Status != "deleted" && id != "manual" {
			live++
			if s.Meta["vigilante.service"] != "order" || s.Meta["vigilante.deployment_id"] != "dep-1" {
				t.Fatalf("snapshot metadata %v", s.Meta)
			}
		}
	}
	if live != 3 || f.Snapshots["manual"].Status == "deleted" {
		t.Fatalf("pruning: %d vigilante snapshots live, manual=%s", live, f.Snapshots["manual"].Status)
	}

	rc := osRC(t, f, "srv-1", cp)
	if err := o.Rollback(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if got := f.Actions(); got != "stop,revert,start" {
		t.Fatalf("actions %s", got)
	}
	if f.Volumes["vol-srv-1"].RevertedTo != cp[cpSnapshot] || f.Volumes["vol-srv-1"].Meta[metaRevertedTo] != cp[cpSnapshot] {
		t.Fatal("volume not reverted to the checkpoint")
	}
	if err := o.Verify(ctx, rc); err != nil {
		t.Fatal(err)
	}
	// Replay after a crash: nothing happens a second time.
	if err := o.Rollback(ctx, rc); err != nil || f.Actions() != "stop,revert,start" {
		t.Fatalf("replay must be a no-op: %v %s", err, f.Actions())
	}
}

func TestOpenStackRevertRefusedLeavesClearError(t *testing.T) {
	f := ostest.New(t)
	f.VolumeServer("srv-2")
	o := osExec(t, "volume")
	cp, err := o.Prepare(context.Background(), osRC(t, f, "srv-2", nil))
	if err != nil {
		t.Fatal(err)
	}
	f.RefuseRevert = true
	err = o.Rollback(context.Background(), osRC(t, f, "srv-2", cp))
	if err == nil || !strings.Contains(err.Error(), "revert-to-snapshot") || !strings.Contains(err.Error(), "left stopped") {
		t.Fatalf("want an actionable revert error, got %v", err)
	}
	if f.Servers["srv-2"].Status != "SHUTOFF" {
		t.Fatalf("server %s", f.Servers["srv-2"].Status)
	}
	if err := o.Verify(context.Background(), osRC(t, f, "srv-2", cp)); err == nil {
		t.Fatal("verify must fail after a refused revert")
	}
}

func TestOpenStackImageSnapshotRebuild(t *testing.T) {
	f := ostest.New(t)
	f.Servers["srv-3"] = &ostest.Server{Status: "ACTIVE", Power: 1, Image: "base-image"}
	o := osExec(t, "auto")
	ctx := context.Background()
	var cp map[string]string
	for i := 0; i < 4; i++ {
		var err error
		if cp, err = o.Prepare(ctx, osRC(t, f, "srv-3", nil)); err != nil {
			t.Fatal(err)
		}
	}
	if cp[cpMode] != "image" || cp[cpImage] == "" {
		t.Fatalf("checkpoint %v", cp)
	}
	live := 0
	for _, im := range f.Images {
		if im.Status != "deleted" {
			live++
		}
	}
	if live != 3 {
		t.Fatalf("image pruning: %d live", live)
	}
	rc := osRC(t, f, "srv-3", cp)
	if err := o.Rollback(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if f.Servers["srv-3"].Image != cp[cpImage] || !strings.HasSuffix(f.Actions(), "rebuild") {
		t.Fatalf("not rebuilt: %s %s", f.Servers["srv-3"].Image, f.Actions())
	}
	if err := o.Verify(ctx, rc); err != nil {
		t.Fatal(err)
	}
	before := f.Actions()
	if err := o.Rollback(ctx, rc); err != nil || f.Actions() != before {
		t.Fatalf("replay must be a no-op: %v", err)
	}
	// Mode mismatches are refused before anything changes.
	if _, err := osExec(t, "volume").Prepare(ctx, osRC(t, f, "srv-3", nil)); err == nil || !strings.Contains(err.Error(), "boots from an image") {
		t.Fatalf("volume mode on an image-booted server: %v", err)
	}
	if err := o.Rollback(ctx, osRC(t, f, "srv-3", nil)); err == nil || !strings.Contains(err.Error(), "prepare") {
		t.Fatalf("rollback without checkpoint: %v", err)
	}
}

func TestOpenStackDryRunAndReauth(t *testing.T) {
	f := ostest.New(t)
	f.VolumeServer("srv-4")
	o := osExec(t, "auto")
	rc := osRC(t, f, "srv-4", nil)
	rc.DryRun = true
	cp, err := o.Prepare(context.Background(), rc)
	if err != nil || len(f.Snapshots) != 0 || cp[cpMode] != "volume" {
		t.Fatalf("dry-run prepare changed something: %v %v", err, f.Snapshots)
	}
	// Tokens revoked server-side: the client logs in again once.
	logins := f.Logins
	f.Mu.Lock()
	f.Tokens = map[string]bool{}
	f.Mu.Unlock()
	if _, err := o.Prepare(context.Background(), osRC(t, f, "srv-4", nil)); err != nil {
		t.Fatal(err)
	}
	if f.Logins != logins+1 {
		t.Fatalf("re-login count %d -> %d", logins, f.Logins)
	}
}

func TestOpenStackPasswordAuthAndInternalInterface(t *testing.T) {
	f := ostest.New(t)
	f.Servers["srv-5"] = &ostest.Server{Status: "ACTIVE", Power: 1, Image: "base"}
	t.Setenv("VGL_TEST_OS_PW", "pw-123456")
	creds := map[string]config.Credential{"os": {Type: "openstack", AuthURL: f.Srv.URL + "/identity/v3", Interface: "internal",
		User: "deployer", PasswordRef: "env:VGL_TEST_OS_PW", ProjectName: "payments"}}
	c, err := openstackClient("os", creds)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := getServer(context.Background(), c, "srv-5"); err != nil {
		t.Fatal(err)
	}
	ep, _ := c.endpoint(context.Background(), svcCompute...)
	if !strings.Contains(ep, "/int/compute") {
		t.Fatalf("internal interface not used: %s", ep)
	}
	scope := f.LastAuth["auth"].(map[string]any)["scope"].(map[string]any)["project"].(map[string]any)
	if scope["name"] != "payments" || scope["domain"].(map[string]any)["name"] != "Default" {
		t.Fatalf("project scope %v", scope)
	}
}

func TestOpenStackDiagnose(t *testing.T) {
	f := ostest.New(t)
	f.VolumeServer("srv-6")
	o := osExec(t, "auto")
	got := map[string]Finding{}
	for _, fd := range o.Diagnose(context.Background(), osRC(t, f, "srv-6", nil)) {
		got[fd.Name] = fd
	}
	for name, want := range map[string]string{"Keystone 로그인": "ok", "서버 조회": "ok", "루트 볼륨": "ok", "Cinder revert API": "ok", "스냅샷 쿼터": "ok"} {
		if got[name].Status != want {
			t.Errorf("%s: %+v", name, got[name])
		}
	}
	if !strings.Contains(got["Cinder revert API"].Detail, "3.70") {
		t.Errorf("max microversion: %+v", got["Cinder revert API"])
	}
	f.QuotaLimit = 0
	for _, fd := range o.Diagnose(context.Background(), osRC(t, f, "srv-6", nil)) {
		if fd.Name == "스냅샷 쿼터" && fd.Status != "fail" {
			t.Errorf("exhausted quota: %+v", fd)
		}
	}
	if fds := o.Diagnose(context.Background(), osRC(t, f, "missing", nil)); fds[len(fds)-1].Status != "fail" {
		t.Errorf("missing server: %+v", fds)
	}
	if !supports("3.70", "3.40") || supports("3.39", "3.40") || !supports("4.0", "3.40") {
		t.Error("microversion compare")
	}
}

// ---------------------------------------------------------------- octavia

func octaviaFor(t *testing.T, f *ostest.Fake, dry bool) TrafficController {
	t.Helper()
	pollInterval = time.Millisecond
	t.Cleanup(func() { pollInterval = 3 * time.Second })
	t.Setenv("VGL_TEST_OS_SECRET", "s3cret-app-cred")
	tc, err := NewTraffic("lb", config.Traffic{Type: "octavia", Octavia: &config.OctaviaTraffic{Credential: "os", PoolID: "pool-1",
		MemberAddress: "{{.Address}}", MemberPort: 8080, WaitTimeout: 10 * time.Second}},
		TrafficEnv{Creds: osCreds(f), DryRun: dry, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return tc
}

func member(addr string) Member {
	return Member{Target: "t-" + addr, Data: tmpl.Data{Address: addr}}
}

func TestOctaviaDrainEnableWaitsForActive(t *testing.T) {
	f := ostest.New(t)
	f.Members["m1"] = &ostest.Member{ID: "m1", Address: "10.0.0.11", Port: 8080, Up: true, Operating: "ONLINE"}
	f.Members["m2"] = &ostest.Member{ID: "m2", Address: "10.0.0.12", Port: 8080, Up: true, Operating: "ONLINE"}
	f.LBStatus, f.LBPending, f.Conflicts = "PENDING_UPDATE", 2, 1 // busy at first, then one stray 409
	tc := octaviaFor(t, f, false)
	ctx := context.Background()

	if id, _ := tc.MemberID(member("10.0.0.11")); id != "10.0.0.11:8080" {
		t.Fatalf("member id %s", id)
	}
	if err := tc.Drain(ctx, []Member{member("10.0.0.11"), member("10.0.0.12")}); err != nil {
		t.Fatal(err)
	}
	if f.Members["m1"].Up || f.Members["m2"].Up || f.Actions() != "member 10.0.0.11 up=false,member 10.0.0.12 up=false" {
		t.Fatalf("drain: %s", f.Actions())
	}
	pool, err := tc.Pool(ctx)
	if err != nil || len(pool) != 2 || pool[0].Enabled || pool[1].Enabled {
		t.Fatalf("pool %v %v", pool, err)
	}
	before := f.Actions()
	if err := tc.Drain(ctx, []Member{member("10.0.0.11")}); err != nil || f.Actions() != before {
		t.Fatalf("drain replay must not change anything: %v", err)
	}
	if err := tc.Enable(ctx, []Member{member("10.0.0.11")}); err != nil {
		t.Fatal(err)
	}
	if !f.Members["m1"].Up || f.Members["m1"].Operating != "ONLINE" {
		t.Fatalf("enable: %+v", f.Members["m1"])
	}
	if err := tc.Drain(ctx, []Member{member("10.9.9.9")}); err == nil || !strings.Contains(err.Error(), "not a member") {
		t.Fatalf("unknown member: %v", err)
	}
	fds := tc.(TrafficDiagnoser).Diagnose(ctx, nil)
	if fds[0].Status != "ok" || !strings.Contains(fds[0].Detail, "lb-1") {
		t.Fatalf("diagnose: %+v", fds)
	}
}

func TestOctaviaDryRun(t *testing.T) {
	f := ostest.New(t)
	f.Members["m1"] = &ostest.Member{ID: "m1", Address: "10.0.0.11", Port: 8080, Up: true, Operating: "ONLINE"}
	if err := octaviaFor(t, f, true).Drain(context.Background(), []Member{member("10.0.0.11")}); err != nil || !f.Members["m1"].Up {
		t.Fatalf("dry-run drained: %v", err)
	}
}
