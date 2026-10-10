//go:build !minimal

package executor

import (
	"context"
	"testing"

	"github.com/vmware/govmomi/simulator"

	"vigilante/internal/config"
)

func TestVSphereSnapshotRevert(t *testing.T) {
	model := simulator.VPX()
	defer model.Remove()
	if err := model.Create(); err != nil {
		t.Fatal(err)
	}
	srv := model.Service.NewServer()
	defer srv.Close()

	ex, _ := New("v", config.Executor{Type: "vsphere", VSphere: &config.VSphereExec{
		URL: srv.URL.String(), VM: "DC0_H0_VM0", Snapshot: "vigilante-{{.Version}}", PowerOn: true, TLSSkipVerify: true}})
	rc := rcFor(nil, "v1")
	ctx := context.Background()
	cp, err := ex.(Preparer).Prepare(ctx, rc)
	if err != nil {
		t.Fatal(err)
	}
	if cp["vsphere.snapshot"] != "vigilante-v2" {
		t.Fatalf("checkpoint %v", cp)
	}
	rc.Checkpoint = cp
	if err := ex.Rollback(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if err := ex.Verify(ctx, rc); err != nil {
		t.Fatal(err)
	}
	// Reverting to a snapshot that does not exist must fail loudly.
	rc.Checkpoint = map[string]string{"vsphere.snapshot": "missing"}
	if err := ex.Rollback(ctx, rc); err == nil {
		t.Fatal("expected error for unknown snapshot")
	}
}
