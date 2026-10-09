package executor

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	elbv2 "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"

	"vigilante/internal/config"
	"vigilante/internal/tmpl"
)

func init() {
	RegisterTraffic("f5", newF5)
	RegisterTraffic("aws_alb", newALB)
}

// ---------------------------------------------------------------------------
// F5 BIG-IP LTM via iControl REST.
//   drain  : session=user-disabled          (no new connections; persistence honoured)
//   offline: session=user-disabled, state=user-down (force_offline: true)
//   enable : session=user-enabled,  state=user-up

type f5Traffic struct {
	spec *config.F5Traffic
	env  TrafficEnv
	http *http.Client

	mu    sync.Mutex
	token string
	exp   time.Time
}

func newF5(_ string, spec config.Traffic, env TrafficEnv) (TrafficController, error) {
	c := env.HTTP
	if c == nil {
		tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: spec.F5.TLSSkipVerify}} //nolint:gosec // opt-in for self-signed BIG-IP certs
		c = &http.Client{Transport: tr, Timeout: 30 * time.Second}
	}
	return &f5Traffic{spec: spec.F5, env: env, http: c}, nil
}

func (f *f5Traffic) MemberID(m Member) (string, error) {
	return tmpl.Render(f.spec.MemberFormat, m.Data)
}

// f5Path converts "/Common/pool" into the iControl "~Common~pool" form.
func f5Path(p string) string { return strings.ReplaceAll(p, "/", "~") }

func (f *f5Traffic) partition() string {
	parts := strings.Split(strings.Trim(f.spec.Pool, "/"), "/")
	if len(parts) > 1 {
		return parts[0]
	}
	return "Common"
}

func (f *f5Traffic) auth(ctx context.Context, req *http.Request) error {
	user, pass, err := basicAuth(f.env.Creds, f.spec.Credential)
	if err != nil {
		return err
	}
	if !f.spec.TokenAuth {
		req.SetBasicAuth(user, pass)
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.token == "" || time.Now().After(f.exp) {
		body, _ := json.Marshal(map[string]string{"username": user, "password": pass, "loginProviderName": "tmos"})
		lreq, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.spec.URL+"/mgmt/shared/authn/login", bytes.NewReader(body))
		lreq.Header.Set("Content-Type", "application/json")
		resp, err := f.http.Do(lreq)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		var out struct {
			Token struct{ Token string } `json:"token"`
		}
		if resp.StatusCode >= 300 || json.NewDecoder(resp.Body).Decode(&out) != nil || out.Token.Token == "" {
			return fmt.Errorf("f5 login failed: %d", resp.StatusCode)
		}
		f.token, f.exp = out.Token.Token, time.Now().Add(15*time.Minute)
	}
	req.Header.Set("X-F5-Auth-Token", f.token)
	return nil
}

func (f *f5Traffic) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(f.spec.URL, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := f.auth(ctx, req); err != nil {
		return err
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("f5 %s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (f *f5Traffic) membersPath() string {
	return "/mgmt/tm/ltm/pool/" + f5Path(f.spec.Pool) + "/members"
}

func (f *f5Traffic) Pool(ctx context.Context) ([]PoolMember, error) {
	var out struct {
		Items []struct {
			Name    string `json:"name"`
			Session string `json:"session"`
			State   string `json:"state"`
		} `json:"items"`
	}
	if err := f.do(ctx, http.MethodGet, f.membersPath(), nil, &out); err != nil {
		return nil, err
	}
	var ms []PoolMember
	for _, it := range out.Items {
		enabled := (it.Session == "monitor-enabled" || it.Session == "user-enabled") && it.State != "user-down"
		ms = append(ms, PoolMember{ID: it.Name, Enabled: enabled})
	}
	return ms, nil
}

func (f *f5Traffic) patch(ctx context.Context, ms []Member, body map[string]string) error {
	var errs []error
	for _, m := range ms {
		id, err := f.MemberID(m)
		if err != nil {
			return err
		}
		path := f.membersPath() + "/~" + f.partition() + "~" + id
		if f.env.DryRun {
			f.env.Log.Info("DRY-RUN: F5 PATCH", "path", path, "body", body)
			continue
		}
		if err := f.do(ctx, http.MethodPatch, path, body, nil); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (f *f5Traffic) Drain(ctx context.Context, ms []Member) error {
	body := map[string]string{"session": "user-disabled"}
	if f.spec.ForceOffline {
		body["state"] = "user-down"
	}
	return f.patch(ctx, ms, body)
}

func (f *f5Traffic) Enable(ctx context.Context, ms []Member) error {
	return f.patch(ctx, ms, map[string]string{"session": "user-enabled", "state": "user-up"})
}

// ---------------------------------------------------------------------------
// AWS ALB / NLB target groups.

// ELBAPI is the subset of the elbv2 client used (mockable).
type ELBAPI interface {
	RegisterTargets(ctx context.Context, in *elbv2.RegisterTargetsInput, opts ...func(*elbv2.Options)) (*elbv2.RegisterTargetsOutput, error)
	DeregisterTargets(ctx context.Context, in *elbv2.DeregisterTargetsInput, opts ...func(*elbv2.Options)) (*elbv2.DeregisterTargetsOutput, error)
	DescribeTargetHealth(ctx context.Context, in *elbv2.DescribeTargetHealthInput, opts ...func(*elbv2.Options)) (*elbv2.DescribeTargetHealthOutput, error)
}

type albTraffic struct {
	spec *config.AWSALBTraffic
	env  TrafficEnv
	api  ELBAPI
	poll time.Duration
}

// NewALBWithAPI is used by tests to inject a fake ELB API.
func NewALBWithAPI(spec *config.AWSALBTraffic, env TrafficEnv, api ELBAPI) TrafficController {
	return &albTraffic{spec: spec, env: env, api: api, poll: 10 * time.Millisecond}
}

func newALB(_ string, spec config.Traffic, env TrafficEnv) (TrafficController, error) {
	var opts []func(*awsconfig.LoadOptions) error
	if c, ok := env.Creds[spec.AWSALB.Credential]; ok {
		if c.Region != "" {
			opts = append(opts, awsconfig.WithRegion(c.Region))
		}
		if c.Profile != "" {
			opts = append(opts, awsconfig.WithSharedConfigProfile(c.Profile))
		}
	}
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, err
	}
	return &albTraffic{spec: spec.AWSALB, env: env, api: elbv2.NewFromConfig(cfg), poll: 5 * time.Second}, nil
}

func (a *albTraffic) MemberID(m Member) (string, error) { return tmpl.Render(a.spec.TargetID, m.Data) }

func (a *albTraffic) descs(ms []Member) ([]elbtypes.TargetDescription, error) {
	var out []elbtypes.TargetDescription
	for _, m := range ms {
		id, err := a.MemberID(m)
		if err != nil {
			return nil, err
		}
		if id == "" || id == "<no value>" {
			return nil, fmt.Errorf("target %s: empty ALB target id (set labels.instance_id or aws_alb.target_id)", m.Target)
		}
		d := elbtypes.TargetDescription{Id: aws.String(id)}
		if a.spec.Port > 0 {
			d.Port = aws.Int32(a.spec.Port)
		}
		out = append(out, d)
	}
	return out, nil
}

func (a *albTraffic) Pool(ctx context.Context) ([]PoolMember, error) {
	out, err := a.api.DescribeTargetHealth(ctx, &elbv2.DescribeTargetHealthInput{TargetGroupArn: aws.String(a.spec.TargetGroupARN)})
	if err != nil {
		return nil, err
	}
	var ms []PoolMember
	for _, d := range out.TargetHealthDescriptions {
		state := elbtypes.TargetHealthStateEnum("")
		if d.TargetHealth != nil {
			state = d.TargetHealth.State
		}
		ms = append(ms, PoolMember{ID: aws.ToString(d.Target.Id), Enabled: state == elbtypes.TargetHealthStateEnumHealthy})
	}
	return ms, nil
}

// waitFor polls DescribeTargetHealth until every target satisfies ok, or timeout.
func (a *albTraffic) waitFor(ctx context.Context, targets []elbtypes.TargetDescription, ok func(elbtypes.TargetHealthStateEnum) bool) error {
	ctx, cancel := context.WithTimeout(ctx, a.spec.WaitTimeout)
	defer cancel()
	for {
		out, err := a.api.DescribeTargetHealth(ctx, &elbv2.DescribeTargetHealthInput{TargetGroupArn: aws.String(a.spec.TargetGroupARN), Targets: targets})
		if err == nil {
			done := true
			for _, d := range out.TargetHealthDescriptions {
				if d.TargetHealth == nil || !ok(d.TargetHealth.State) {
					done = false
				}
			}
			if done {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for target group state: %w", ctx.Err())
		case <-time.After(a.poll):
		}
	}
}

func (a *albTraffic) Drain(ctx context.Context, ms []Member) error {
	targets, err := a.descs(ms)
	if err != nil {
		return err
	}
	if a.env.DryRun {
		a.env.Log.Info("DRY-RUN: ALB DeregisterTargets", "tg", a.spec.TargetGroupARN, "targets", len(targets))
		return nil
	}
	if _, err := a.api.DeregisterTargets(ctx, &elbv2.DeregisterTargetsInput{TargetGroupArn: aws.String(a.spec.TargetGroupARN), Targets: targets}); err != nil {
		return err
	}
	// Wait until connection draining (deregistration_delay) completes. A slow
	// drain is not fatal: the target receives no new requests already.
	err = a.waitFor(ctx, targets, func(s elbtypes.TargetHealthStateEnum) bool {
		return s == elbtypes.TargetHealthStateEnumUnused
	})
	if err != nil {
		a.env.Log.Warn("ALB drain not complete; continuing", "err", err)
	}
	return nil
}

func (a *albTraffic) Enable(ctx context.Context, ms []Member) error {
	targets, err := a.descs(ms)
	if err != nil {
		return err
	}
	if a.env.DryRun {
		a.env.Log.Info("DRY-RUN: ALB RegisterTargets", "tg", a.spec.TargetGroupARN, "targets", len(targets))
		return nil
	}
	if _, err := a.api.RegisterTargets(ctx, &elbv2.RegisterTargetsInput{TargetGroupArn: aws.String(a.spec.TargetGroupARN), Targets: targets}); err != nil {
		return err
	}
	return a.waitFor(ctx, targets, func(s elbtypes.TargetHealthStateEnum) bool {
		return s == elbtypes.TargetHealthStateEnumHealthy
	})
}
