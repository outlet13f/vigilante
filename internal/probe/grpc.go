package probe

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"vigilante/internal/config"
	"vigilante/internal/tmpl"
)

func init() { Register("grpc", newGRPC) }

// gRPC probe uses the standard grpc.health.v1 protocol.
// Metrics: up, latency_ms, consecutive_failures, consecutive_timeouts.
type grpcProbe struct {
	spec config.Probe
	conn *grpc.ClientConn
	fc   failureCounter
}

func newGRPC(spec config.Probe, env Env) (Probe, error) {
	addr, err := tmpl.Render(spec.GRPC.Address, env.Data)
	if err != nil {
		return nil, err
	}
	creds := insecure.NewCredentials()
	if spec.GRPC.TLS {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, err
	}
	return &grpcProbe{spec: spec, conn: conn}, nil
}

func (p *grpcProbe) Run(ctx context.Context, emit Emit) error {
	defer p.conn.Close()
	return poll(ctx, p.spec.Interval, func(ctx context.Context) {
		start := time.Now()
		err := p.Check(ctx)
		if err == nil {
			emit("latency_ms", ms(time.Since(start)))
		}
		timeout := err != nil && (isTimeout(err) || status.Code(err) == codes.DeadlineExceeded)
		p.fc.observe(emit, err == nil, timeout)
	})
}

func (p *grpcProbe) Close() error { return p.conn.Close() }

func (p *grpcProbe) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, p.spec.Timeout)
	defer cancel()
	resp, err := healthpb.NewHealthClient(p.conn).Check(ctx, &healthpb.HealthCheckRequest{Service: p.spec.GRPC.Service})
	if err != nil {
		return err
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		return fmt.Errorf("grpc health status %s", resp.GetStatus())
	}
	return nil
}
