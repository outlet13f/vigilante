//go:build !minimal

package executor

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	elbv2 "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"

	"vigilante/internal/config"
	"vigilante/internal/tmpl"
)

// Not in the minimal build (-tags minimal): the AWS SDK is large.
func init() { RegisterTraffic("aws_alb", newALB) }

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
