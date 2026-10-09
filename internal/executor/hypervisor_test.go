package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func TestNutanixRestore(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+strings.TrimPrefix(r.URL.Path, "/PrismGateway/services/rest/v2.0"))
		mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, "/PrismGateway/services/rest/v2.0")
		switch {
		case strings.HasPrefix(p, "/tasks/"):
			io.WriteString(w, `{"progress_status":"Succeeded"}`)
		case p == "/snapshots/" && r.Method == http.MethodPost:
			io.WriteString(w, `{"task_uuid":"t1"}`)
		case p == "/snapshots/":
			io.WriteString(w, `{"entities":[{"uuid":"snap-1","snapshot_name":"vigilante-order-v2","vm_uuid":"vm-1"}]}`)
		case p == "/vms/vm-1/restore":
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			if b["snapshot_uuid"] != "snap-1" {
				w.WriteHeader(400)
				return
			}
			io.WriteString(w, `{"task_uuid":"t2"}`)
		case p == "/vms/vm-1/set_power_state":
			io.WriteString(w, `{"task_uuid":"t3"}`)
		case p == "/vms/vm-1":
			io.WriteString(w, `{"power_state":"on"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	ex, _ := New("n", config.Executor{Type: "nutanix", Nutanix: &config.NutanixExec{
		URL: srv.URL, VMUUID: "vm-1", Snapshot: "vigilante-{{.Service}}-{{.Version}}", TLSSkipVerify: true}})
	rc := rcFor(nil, "v1")
	ctx := context.Background()
	cp, err := ex.(Preparer).Prepare(ctx, rc)
	if err != nil || cp["nutanix.snapshot_uuid"] != "snap-1" {
		t.Fatalf("prepare %v %v", cp, err)
	}
	rc.Checkpoint = cp
	if err := ex.Rollback(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if err := ex.Verify(ctx, rc); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(calls, ",")
	if !strings.Contains(joined, "POST /vms/vm-1/restore") || !strings.Contains(joined, "POST /vms/vm-1/set_power_state") {
		t.Fatal(joined)
	}
}
