package executor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/tmpl"
)

// ---------------------------------------------------------------------------
// Strategy C on OpenStack Octavia (v2): a member is drained with
// admin_state_up=false (existing connections finish, no new ones) and
// returned with admin_state_up=true. Octavia accepts one change per load
// balancer at a time: while it is PENDING_* a change gets 409, so every
// change waits for ACTIVE first, retries 409, and is applied one by one.

func init() { RegisterTraffic("octavia", newOctavia) }

type octaviaTraffic struct {
	spec *config.OctaviaTraffic
	env  TrafficEnv
}

func newOctavia(_ string, spec config.Traffic, env TrafficEnv) (TrafficController, error) {
	return &octaviaTraffic{spec: spec.Octavia, env: env}, nil
}

type octMember struct {
	ID                 string `json:"id"`
	Address            string `json:"address"`
	ProtocolPort       int    `json:"protocol_port"`
	AdminStateUp       bool   `json:"admin_state_up"`
	Weight             *int   `json:"weight"`
	OperatingStatus    string `json:"operating_status"`
	ProvisioningStatus string `json:"provisioning_status"`
}

func (o *octaviaTraffic) client() (*osClient, error) {
	return openstackClient(o.spec.Credential, o.env.Creds)
}

func (o *octaviaTraffic) MemberID(m Member) (string, error) {
	addr, err := tmpl.Render(o.spec.MemberAddress, m.Data)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(addr, strconv.Itoa(o.spec.MemberPort)), nil
}

func (o *octaviaTraffic) poolPath() string { return "/v2/lbaas/pools/" + url.PathEscape(o.spec.PoolID) }

func (o *octaviaTraffic) members(ctx context.Context, c *osClient) ([]octMember, error) {
	var out struct {
		Members []octMember `json:"members"`
	}
	if _, err := c.call(ctx, svcLB, http.MethodGet, o.poolPath()+"/members", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Members, nil
}

func (o *octaviaTraffic) Pool(ctx context.Context) ([]PoolMember, error) {
	c, err := o.client()
	if err != nil {
		return nil, err
	}
	ms, err := o.members(ctx, c)
	if err != nil {
		return nil, err
	}
	out := make([]PoolMember, 0, len(ms))
	for _, m := range ms {
		enabled := m.AdminStateUp && (m.Weight == nil || *m.Weight > 0)
		out = append(out, PoolMember{ID: net.JoinHostPort(m.Address, strconv.Itoa(m.ProtocolPort)), Enabled: enabled})
	}
	return out, nil
}

// loadBalancer returns the pool's load balancer ID.
func (o *octaviaTraffic) loadBalancer(ctx context.Context, c *osClient) (string, error) {
	var out struct {
		Pool struct {
			LoadBalancers []struct {
				ID string `json:"id"`
			} `json:"loadbalancers"`
		} `json:"pool"`
	}
	if _, err := c.call(ctx, svcLB, http.MethodGet, o.poolPath(), nil, nil, &out); err != nil {
		return "", err
	}
	if len(out.Pool.LoadBalancers) == 0 {
		return "", fmt.Errorf("octavia pool %s belongs to no load balancer", o.spec.PoolID)
	}
	return out.Pool.LoadBalancers[0].ID, nil
}

// waitActive waits until the load balancer accepts changes.
func (o *octaviaTraffic) waitActive(ctx context.Context, c *osClient, lb string) error {
	return poll(ctx, pollInterval, "load balancer "+lb+" ACTIVE", func() (bool, error) {
		var out struct {
			LoadBalancer struct {
				ProvisioningStatus string `json:"provisioning_status"`
			} `json:"loadbalancer"`
		}
		if _, err := c.call(ctx, svcLB, http.MethodGet, "/v2/lbaas/loadbalancers/"+url.PathEscape(lb), nil, nil, &out); err != nil {
			return false, err
		}
		switch st := out.LoadBalancer.ProvisioningStatus; st {
		case "ACTIVE":
			return true, nil
		case "ERROR", "DELETED":
			return false, fmt.Errorf("load balancer %s is %s", lb, st)
		}
		return false, nil
	})
}

// set changes admin_state_up of each member, one at a time.
func (o *octaviaTraffic) set(ctx context.Context, ms []Member, up bool) error {
	c, err := o.client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, o.spec.WaitTimeout)
	defer cancel()
	current, err := o.members(ctx, c)
	if err != nil {
		return err
	}
	byAddr := map[string]octMember{}
	for _, m := range current {
		byAddr[net.JoinHostPort(m.Address, strconv.Itoa(m.ProtocolPort))] = m
	}
	lb, err := o.loadBalancer(ctx, c)
	if err != nil {
		return err
	}
	var errs []error
	var changed []string
	for _, m := range ms {
		key, err := o.MemberID(m)
		if err != nil {
			return err
		}
		om, ok := byAddr[key]
		if !ok {
			errs = append(errs, fmt.Errorf("%s (%s) is not a member of octavia pool %s", m.Target, key, o.spec.PoolID))
			continue
		}
		if om.AdminStateUp == up {
			continue // already there (idempotent replay)
		}
		if o.env.DryRun {
			o.env.Log.Info("DRY-RUN: octavia member admin_state_up", "member", key, "up", up)
			continue
		}
		path := o.poolPath() + "/members/" + url.PathEscape(om.ID)
		body := map[string]any{"member": map[string]any{"admin_state_up": up}}
		err = poll(ctx, pollInterval, "octavia member update "+key, func() (bool, error) {
			if err := o.waitActive(ctx, c, lb); err != nil {
				return false, err
			}
			_, err := c.call(ctx, svcLB, http.MethodPut, path, nil, body, nil)
			if isStatus(err, http.StatusConflict) { // a change slipped in between: wait and retry
				return false, nil
			}
			return err == nil, err
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			continue
		}
		changed = append(changed, om.ID)
	}
	if len(changed) > 0 {
		if err := o.waitActive(ctx, c, lb); err != nil {
			errs = append(errs, err)
		}
	}
	if up {
		for _, id := range changed {
			if err := o.waitOnline(ctx, c, id); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// waitOnline waits for a re-enabled member to pass its health monitor.
func (o *octaviaTraffic) waitOnline(ctx context.Context, c *osClient, id string) error {
	return poll(ctx, pollInterval, "member "+id+" ONLINE", func() (bool, error) {
		var out struct {
			Member octMember `json:"member"`
		}
		if _, err := c.call(ctx, svcLB, http.MethodGet, o.poolPath()+"/members/"+url.PathEscape(id), nil, nil, &out); err != nil {
			return false, err
		}
		switch out.Member.OperatingStatus {
		case "ONLINE", "NO_MONITOR":
			return true, nil
		case "ERROR":
			return false, fmt.Errorf("member %s operating_status ERROR (health monitor failing)", id)
		}
		return false, nil
	})
}

func (o *octaviaTraffic) Drain(ctx context.Context, ms []Member) error  { return o.set(ctx, ms, false) }
func (o *octaviaTraffic) Enable(ctx context.Context, ms []Member) error { return o.set(ctx, ms, true) }

// Diagnose checks login and that the pool and its load balancer are usable.
func (o *octaviaTraffic) Diagnose(ctx context.Context, _ []Member) []Finding {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, err := o.client()
	if err != nil {
		return []Finding{finding("OpenStack 자격증명", err, "")}
	}
	if _, err := c.session(ctx, false); err != nil {
		return []Finding{finding("Keystone 로그인", err, "")}
	}
	lb, err := o.loadBalancer(ctx, c)
	if err != nil {
		return []Finding{finding("Octavia 풀", err, "")}
	}
	var out struct {
		LoadBalancer struct {
			ProvisioningStatus string `json:"provisioning_status"`
			OperatingStatus    string `json:"operating_status"`
		} `json:"loadbalancer"`
	}
	if _, err := c.call(ctx, svcLB, http.MethodGet, "/v2/lbaas/loadbalancers/"+url.PathEscape(lb), nil, nil, &out); err != nil {
		return []Finding{finding("Octavia 로드밸런서", err, "")}
	}
	st := out.LoadBalancer.ProvisioningStatus
	f := Finding{Name: "Octavia 로드밸런서", Status: "ok", Detail: fmt.Sprintf("%s provisioning=%s operating=%s", lb, st, out.LoadBalancer.OperatingStatus)}
	if st != "ACTIVE" {
		f.Status = "warn"
		f.Detail += " (ACTIVE가 아니면 멤버 변경이 대기함)"
	}
	return []Finding{f, {Name: "멤버 수정 권한", Status: "warn",
		Detail: "변경 없이는 확인할 수 없음: 프로젝트 역할에 load-balancer_member(또는 member) 권한이 있는지 확인"}}
}
