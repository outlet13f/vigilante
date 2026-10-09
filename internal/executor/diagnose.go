package executor

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	q "vigilante/internal/transport"
)

// symlink: the link, the releases directory and (when known) the previous
// release must exist, and the service unit must be manageable.
func (s *symlinkExec) Diagnose(ctx context.Context, rc *RunContext) []Finding {
	r, err := rc.runner()
	if err != nil {
		return []Finding{{Name: "symlink: 접속", Status: "fail", Detail: err.Error()}}
	}
	var out []Finding
	link, _ := rc.render(s.spec.Link)
	cur, err := r.Run(ctx, "readlink -f "+q.ShellQuote(link), nil)
	out = append(out, finding("symlink: 현재 링크 "+link, err, strings.TrimSpace(cur)))
	dir, _ := rc.render(s.spec.ReleasesDir)
	_, err = r.Run(ctx, "test -d "+q.ShellQuote(dir), nil)
	out = append(out, finding("symlink: 릴리스 디렉토리 "+dir, err, "있음"))
	if _, target, err := s.release(rc); err != nil {
		out = append(out, Finding{Name: "symlink: 이전 릴리스", Status: "skip", Detail: "이전 버전 미지정 (--previous)"})
	} else {
		_, err = r.Run(ctx, "test -d "+q.ShellQuote(target), nil)
		out = append(out, finding("symlink: 이전 릴리스 "+target, err, "있음"))
	}
	if s.spec.Init == "systemd" && s.spec.RestartCmd == "" {
		unit, _ := rc.render(s.spec.Unit)
		st, err := r.Run(ctx, "systemctl is-active "+q.ShellQuote(unit), nil)
		st = strings.TrimSpace(st)
		if err != nil && st == "" {
			out = append(out, finding("symlink: 서비스 "+unit, err, ""))
		} else if st != "active" {
			out = append(out, Finding{Name: "symlink: 서비스 " + unit, Status: "warn", Detail: "상태 " + st})
		} else {
			out = append(out, Finding{Name: "symlink: 서비스 " + unit, Status: "ok", Detail: st})
		}
	}
	return out
}

// container: the container must be visible through the engine socket and the
// previous image should already be on the host (or pull must be enabled).
func (c *containerExec) Diagnose(ctx context.Context, rc *RunContext) []Finding {
	name, _ := rc.render(c.spec.Name)
	cl := c.newClient(rc)
	ct, err := cl.Inspect(ctx, name)
	if err != nil {
		return []Finding{finding("container: "+name+" 조회", err, "")}
	}
	out := []Finding{{Name: "container: " + name + " 조회", Status: "ok", Detail: ct.Config.Image + ", " + ct.State.Status}}
	tag, err := rc.render(c.spec.Tag)
	if err != nil || tag == "" {
		return append(out, Finding{Name: "container: 이전 이미지", Status: "skip", Detail: "이전 버전 미지정 (--previous)"})
	}
	image := c.targetImage(rc, ct, tag)
	ok, err := cl.ImageExists(ctx, image)
	switch {
	case err != nil:
		out = append(out, finding("container: 이전 이미지 "+image, err, ""))
	case ok:
		out = append(out, Finding{Name: "container: 이전 이미지 " + image, Status: "ok", Detail: "호스트에 있음"})
	case c.spec.Pull:
		out = append(out, Finding{Name: "container: 이전 이미지 " + image, Status: "warn", Detail: "호스트에 없음, 롤백 시 pull 필요(레지스트리 접근 필요)"})
	default:
		out = append(out, Finding{Name: "container: 이전 이미지 " + image, Status: "fail", Detail: "호스트에 없고 pull이 꺼져 있음"})
	}
	return out
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

func (n *nutanixExec) Diagnose(ctx context.Context, rc *RunContext) []Finding {
	vm, _, err := n.ids(rc)
	if err == nil {
		err = n.call(ctx, rc, http.MethodGet, "/vms/"+vm, nil, nil)
	}
	return []Finding{finding("nutanix: Prism 접속·VM 조회", err, vm)}
}

func (k *kvmExec) Diagnose(ctx context.Context, rc *RunContext) []Finding {
	r, err := rc.runnerFor(k.spec.Hypervisor)
	if err != nil {
		return []Finding{finding("kvm: 하이퍼바이저 접속", err, "")}
	}
	dom, _, _ := k.names(rc)
	out, err := r.Run(ctx, "virsh domstate "+q.ShellQuote(dom), nil)
	return []Finding{finding("kvm: 도메인 "+dom, err, strings.TrimSpace(out))}
}

// nginx: every LB host must let us rewrite the upstream file and pass nginx -t.
func (n *nginxTraffic) Diagnose(ctx context.Context, _ []Member) []Finding {
	var out []Finding
	for _, host := range n.spec.Hosts {
		r, err := n.env.Runners(host)
		if err != nil {
			out = append(out, finding("nginx: "+host+" 접속", err, ""))
			continue
		}
		_, err = r.Run(ctx, "test -w "+q.ShellQuote(n.spec.UpstreamFile), nil)
		out = append(out, finding(fmt.Sprintf("nginx: %s 파일 쓰기 권한", host), err, n.spec.UpstreamFile))
		_, err = r.Run(ctx, n.spec.TestCmd+" 2>&1", nil)
		out = append(out, finding(fmt.Sprintf("nginx: %s 설정 검사", host), err, n.spec.TestCmd))
	}
	return out
}

func (f *f5Traffic) Diagnose(context.Context, []Member) []Finding {
	return []Finding{{Name: "f5: 쓰기 권한", Status: "warn", Detail: "풀 멤버 변경 권한은 변경 없이 확인할 수 없음 (조회 권한만 확인됨)"}}
}
