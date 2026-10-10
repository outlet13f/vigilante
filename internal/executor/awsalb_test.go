//go:build !minimal

package executor

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	elbv2 "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"

	"vigilante/internal/config"
)

type fakeELB struct {
	mu         sync.Mutex
	registered map[string]bool
	calls      []string
}

func (f *fakeELB) RegisterTargets(_ context.Context, in *elbv2.RegisterTargetsInput, _ ...func(*elbv2.Options)) (*elbv2.RegisterTargetsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range in.Targets {
		f.registered[aws.ToString(t.Id)] = true
		f.calls = append(f.calls, "register:"+aws.ToString(t.Id))
	}
	return &elbv2.RegisterTargetsOutput{}, nil
}

func (f *fakeELB) DeregisterTargets(_ context.Context, in *elbv2.DeregisterTargetsInput, _ ...func(*elbv2.Options)) (*elbv2.DeregisterTargetsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range in.Targets {
		delete(f.registered, aws.ToString(t.Id))
		f.calls = append(f.calls, "deregister:"+aws.ToString(t.Id))
	}
	return &elbv2.DeregisterTargetsOutput{}, nil
}

func (f *fakeELB) DescribeTargetHealth(_ context.Context, in *elbv2.DescribeTargetHealthInput, _ ...func(*elbv2.Options)) (*elbv2.DescribeTargetHealthOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := []string{"i-1", "i-2"}
	if len(in.Targets) > 0 {
		ids = nil
		for _, t := range in.Targets {
			ids = append(ids, aws.ToString(t.Id))
		}
	}
	out := &elbv2.DescribeTargetHealthOutput{}
	for _, id := range ids {
		state := elbtypes.TargetHealthStateEnumUnused
		if f.registered[id] {
			state = elbtypes.TargetHealthStateEnumHealthy
		}
		out.TargetHealthDescriptions = append(out.TargetHealthDescriptions, elbtypes.TargetHealthDescription{
			Target: &elbtypes.TargetDescription{Id: aws.String(id)}, TargetHealth: &elbtypes.TargetHealth{State: state}})
	}
	return out, nil
}

func TestALB(t *testing.T) {
	api := &fakeELB{registered: map[string]bool{"i-1": true, "i-2": true}}
	tc := NewALBWithAPI(&config.AWSALBTraffic{TargetGroupARN: "arn:tg", TargetID: "{{.Labels.instance_id}}", WaitTimeout: 1e9}, TrafficEnv{Log: testLog}, api)
	m := []Member{{Target: "app-1", Data: rcFor(nil, "").Data}}
	if err := tc.Drain(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	pool, _ := tc.Pool(context.Background())
	if pool[0].Enabled || !pool[1].Enabled {
		t.Fatalf("after drain %+v", pool)
	}
	if err := tc.Enable(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if strings.Join(api.calls, ",") != "deregister:i-1,register:i-1" {
		t.Fatalf("calls %v", api.calls)
	}
}
