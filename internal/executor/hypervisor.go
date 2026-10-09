package executor

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"

	"vigilante/internal/config"
	q "vigilante/internal/transport"
)

func init() {
	Register("vsphere", newVSphere)
	Register("nutanix", newNutanix)
	Register("kvm", newKVM)
}

// ---------------------------------------------------------------------------
// Strategy D (VMware vSphere / ESXi) — snapshot taken by `prepare`, reverted on
// rollback. Uses the vSphere Web Services API through govmomi.

type vsphereExec struct{ spec *config.VSphereExec }

func newVSphere(_ string, spec config.Executor) (Executor, error) {
	return &vsphereExec{spec: spec.VSphere}, nil
}

func (v *vsphereExec) connect(ctx context.Context, rc *RunContext) (*govmomi.Client, *object.VirtualMachine, string, error) {
	u, err := url.Parse(v.spec.URL)
	if err != nil {
		return nil, nil, "", err
	}
	user, pass, err := basicAuth(rc.Creds, v.spec.Credential)
	if err != nil {
		return nil, nil, "", err
	}
	if user != "" {
		u.User = url.UserPassword(user, pass)
	}
	c, err := govmomi.NewClient(ctx, u, v.spec.TLSSkipVerify)
	if err != nil {
		return nil, nil, "", fmt.Errorf("vsphere login: %w", err)
	}
	vmName, err := rc.render(v.spec.VM)
	if err != nil {
		return nil, nil, "", err
	}
	snap, err := rc.render(v.spec.Snapshot)
	if err != nil {
		return nil, nil, "", err
	}
	f := find.NewFinder(c.Client, true)
	dc, err := f.DefaultDatacenter(ctx)
	if err == nil {
		f.SetDatacenter(dc)
	}
	vm, err := f.VirtualMachine(ctx, vmName)
	if err != nil {
		_ = c.Logout(ctx)
		return nil, nil, "", fmt.Errorf("find vm %s: %w", vmName, err)
	}
	return c, vm, snap, nil
}

func (v *vsphereExec) Prepare(ctx context.Context, rc *RunContext) (map[string]string, error) {
	c, vm, snap, err := v.connect(ctx, rc)
	if err != nil {
		return nil, err
	}
	defer c.Logout(ctx)
	if rc.DryRun {
		return map[string]string{"vsphere.snapshot": snap}, nil
	}
	task, err := vm.CreateSnapshot(ctx, snap, "vigilante pre-deploy checkpoint", false, true)
	if err != nil {
		return nil, err
	}
	if err := task.Wait(ctx); err != nil {
		return nil, fmt.Errorf("create snapshot %s: %w", snap, err)
	}
	return map[string]string{"vsphere.snapshot": snap}, nil
}

func (v *vsphereExec) Rollback(ctx context.Context, rc *RunContext) error {
	c, vm, snap, err := v.connect(ctx, rc)
	if err != nil {
		return err
	}
	defer c.Logout(ctx)
	if s := rc.Checkpoint["vsphere.snapshot"]; s != "" {
		snap = s
	}
	if rc.DryRun {
		rc.Log.Info("DRY-RUN: would revert VM snapshot", "vm", vm.InventoryPath, "snapshot", snap)
		return nil
	}
	task, err := vm.RevertToSnapshot(ctx, snap, !v.spec.PowerOn)
	if err != nil {
		return err
	}
	if err := task.Wait(ctx); err != nil {
		return fmt.Errorf("revert %s to %s: %w", vm.InventoryPath, snap, err)
	}
	if v.spec.PowerOn {
		state, err := vm.PowerState(ctx)
		if err == nil && state != types.VirtualMachinePowerStatePoweredOn {
			if t, err := vm.PowerOn(ctx); err == nil {
				_ = t.Wait(ctx)
			}
		}
	}
	return nil
}

func (v *vsphereExec) Verify(ctx context.Context, rc *RunContext) error {
	if rc.DryRun || !v.spec.PowerOn {
		return nil
	}
	c, vm, _, err := v.connect(ctx, rc)
	if err != nil {
		return err
	}
	defer c.Logout(ctx)
	state, err := vm.PowerState(ctx)
	if err != nil {
		return err
	}
	if state != types.VirtualMachinePowerStatePoweredOn {
		return fmt.Errorf("vm %s is %s after revert", vm.InventoryPath, state)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Strategy D (Nutanix AHV) — Prism Element REST v2.0: snapshots + VM restore.
// Endpoint paths follow AOS 5.x/6.x PE v2; confirm against your AOS release.

type nutanixExec struct {
	spec *config.NutanixExec
	http *http.Client
}

func newNutanix(_ string, spec config.Executor) (Executor, error) {
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: spec.Nutanix.TLSSkipVerify}} //nolint:gosec // opt-in
	return &nutanixExec{spec: spec.Nutanix, http: &http.Client{Transport: tr, Timeout: 30 * time.Second}}, nil
}

func (n *nutanixExec) call(ctx context.Context, rc *RunContext, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(n.spec.URL, "/")+"/PrismGateway/services/rest/v2.0"+path, rd)
	if err != nil {
		return err
	}
	user, pass, err := basicAuth(rc.Creds, n.spec.Credential)
	if err != nil {
		return err
	}
	req.SetBasicAuth(user, pass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("prism %s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (n *nutanixExec) waitTask(ctx context.Context, rc *RunContext, uuid string) error {
	for {
		var t struct {
			ProgressStatus string `json:"progress_status"`
			MetaResponse   struct {
				ErrorDetail string `json:"error_detail"`
			} `json:"meta_response"`
		}
		if err := n.call(ctx, rc, http.MethodGet, "/tasks/"+uuid, nil, &t); err != nil {
			return err
		}
		switch strings.ToLower(t.ProgressStatus) {
		case "succeeded":
			return nil
		case "failed", "aborted":
			return fmt.Errorf("prism task %s %s: %s", uuid, t.ProgressStatus, t.MetaResponse.ErrorDetail)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (n *nutanixExec) ids(rc *RunContext) (vm, snap string, err error) {
	if vm, err = rc.render(n.spec.VMUUID); err != nil {
		return
	}
	snap, err = rc.render(n.spec.Snapshot)
	return
}

func (n *nutanixExec) Prepare(ctx context.Context, rc *RunContext) (map[string]string, error) {
	vm, snap, err := n.ids(rc)
	if err != nil {
		return nil, err
	}
	if rc.DryRun {
		return map[string]string{"nutanix.snapshot_name": snap}, nil
	}
	var task struct {
		TaskUUID string `json:"task_uuid"`
	}
	body := map[string]any{"snapshot_specs": []map[string]string{{"vm_uuid": vm, "snapshot_name": snap}}}
	if err := n.call(ctx, rc, http.MethodPost, "/snapshots/", body, &task); err != nil {
		return nil, err
	}
	if err := n.waitTask(ctx, rc, task.TaskUUID); err != nil {
		return nil, err
	}
	uuid, err := n.findSnapshot(ctx, rc, vm, snap)
	if err != nil {
		return nil, err
	}
	return map[string]string{"nutanix.snapshot_name": snap, "nutanix.snapshot_uuid": uuid}, nil
}

func (n *nutanixExec) findSnapshot(ctx context.Context, rc *RunContext, vm, name string) (string, error) {
	var list struct {
		Entities []struct {
			UUID         string `json:"uuid"`
			SnapshotName string `json:"snapshot_name"`
			VMUUID       string `json:"vm_uuid"`
		} `json:"entities"`
	}
	if err := n.call(ctx, rc, http.MethodGet, "/snapshots/?vm_uuid="+url.QueryEscape(vm), nil, &list); err != nil {
		return "", err
	}
	for _, e := range list.Entities {
		if e.SnapshotName == name && (e.VMUUID == "" || e.VMUUID == vm) {
			return e.UUID, nil
		}
	}
	return "", fmt.Errorf("snapshot %q not found for vm %s", name, vm)
}

func (n *nutanixExec) Rollback(ctx context.Context, rc *RunContext) error {
	vm, snap, err := n.ids(rc)
	if err != nil {
		return err
	}
	snapUUID := rc.Checkpoint["nutanix.snapshot_uuid"]
	if snapUUID == "" {
		if snapUUID, err = n.findSnapshot(ctx, rc, vm, snap); err != nil {
			return err
		}
	}
	if rc.DryRun {
		rc.Log.Info("DRY-RUN: would restore Nutanix VM", "vm", vm, "snapshot", snapUUID)
		return nil
	}
	var task struct {
		TaskUUID string `json:"task_uuid"`
	}
	body := map[string]any{"snapshot_uuid": snapUUID, "restore_network_configuration": true}
	if err := n.call(ctx, rc, http.MethodPost, "/vms/"+vm+"/restore", body, &task); err != nil {
		return err
	}
	if err := n.waitTask(ctx, rc, task.TaskUUID); err != nil {
		return err
	}
	// Restore leaves the VM powered off.
	if err := n.call(ctx, rc, http.MethodPost, "/vms/"+vm+"/set_power_state", map[string]string{"transition": "ON"}, &task); err != nil {
		return err
	}
	return n.waitTask(ctx, rc, task.TaskUUID)
}

func (n *nutanixExec) Verify(ctx context.Context, rc *RunContext) error {
	if rc.DryRun {
		return nil
	}
	vm, _, err := n.ids(rc)
	if err != nil {
		return err
	}
	var info struct {
		PowerState string `json:"power_state"`
	}
	if err := n.call(ctx, rc, http.MethodGet, "/vms/"+vm, nil, &info); err != nil {
		return err
	}
	if !strings.EqualFold(info.PowerState, "on") {
		return fmt.Errorf("vm %s power_state=%s", vm, info.PowerState)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Strategy D (KVM / libvirt) — virsh over SSH on the hypervisor host.

type kvmExec struct{ spec *config.KVMExec }

func newKVM(_ string, spec config.Executor) (Executor, error) { return &kvmExec{spec: spec.KVM}, nil }

func (k *kvmExec) names(rc *RunContext) (dom, snap string, err error) {
	if dom, err = rc.render(k.spec.Domain); err != nil {
		return
	}
	snap, err = rc.render(k.spec.Snapshot)
	if s := rc.Checkpoint["kvm.snapshot"]; s != "" {
		snap = s
	}
	return
}

func (k *kvmExec) Prepare(ctx context.Context, rc *RunContext) (map[string]string, error) {
	r, err := rc.runnerFor(k.spec.Hypervisor)
	if err != nil {
		return nil, err
	}
	dom, snap, err := k.names(rc)
	if err != nil {
		return nil, err
	}
	cmd := fmt.Sprintf("virsh snapshot-create-as --domain %s --name %s --description %s --atomic",
		q.ShellQuote(dom), q.ShellQuote(snap), q.ShellQuote("vigilante pre-deploy checkpoint"))
	if _, err := r.Run(ctx, cmd, nil); err != nil {
		return nil, err
	}
	return map[string]string{"kvm.snapshot": snap}, nil
}

func (k *kvmExec) Rollback(ctx context.Context, rc *RunContext) error {
	r, err := rc.runnerFor(k.spec.Hypervisor)
	if err != nil {
		return err
	}
	dom, snap, err := k.names(rc)
	if err != nil {
		return err
	}
	_, err = r.Run(ctx, fmt.Sprintf("virsh snapshot-revert --domain %s --snapshotname %s --running --force",
		q.ShellQuote(dom), q.ShellQuote(snap)), nil)
	return err
}

func (k *kvmExec) Verify(ctx context.Context, rc *RunContext) error {
	if rc.DryRun {
		return nil
	}
	r, err := rc.runnerFor(k.spec.Hypervisor)
	if err != nil {
		return err
	}
	dom, _, err := k.names(rc)
	if err != nil {
		return err
	}
	out, err := r.Run(ctx, "virsh domstate "+q.ShellQuote(dom), nil)
	if err != nil {
		return err
	}
	if s := strings.TrimSpace(out); s != "running" {
		return fmt.Errorf("domain %s is %q", dom, s)
	}
	return nil
}
