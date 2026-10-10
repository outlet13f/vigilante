//go:build !minimal

package executor

import (
	"context"
	"fmt"
	"net/url"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"

	"vigilante/internal/config"
)

// Not in the minimal build (-tags minimal): govmomi is the largest dependency.
func init() { Register("vsphere", newVSphere) }

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

func (v *vsphereExec) Diagnose(ctx context.Context, rc *RunContext) []Finding {
	c, vm, snap, err := v.connect(ctx, rc)
	if err != nil {
		return []Finding{finding("vsphere: 로그인·VM 조회", err, "")}
	}
	defer c.Logout(ctx)
	return []Finding{{Name: "vsphere: 로그인·VM 조회", Status: "ok", Detail: vm.InventoryPath},
		{Name: "vsphere: 스냅샷 권한", Status: "warn", Detail: "생성·복원 권한은 변경 없이 확인할 수 없음 (prepare에서 " + snap + " 생성으로 확인)"}}
}
