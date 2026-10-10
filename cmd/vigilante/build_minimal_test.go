//go:build minimal

package main

import (
	"strings"
	"testing"
)

// The reference config uses vSphere, AWS ALB and gRPC: the minimal build
// must refuse it by name instead of failing at rollback time.
func TestMinimalBuildRefusesMissingPlugins(t *testing.T) {
	_, err := loadConfig("../../examples/config/vigilante.yaml")
	if err == nil {
		t.Fatal("minimal build accepted a config using vsphere/aws_alb/grpc")
	}
	for _, want := range []string{"type vsphere", "type aws_alb", "type grpc", "full build"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}
