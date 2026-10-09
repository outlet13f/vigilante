package transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
)

// Mock is a scripted Runner for tests: each command is matched against
// Responses by substring (first match wins) and recorded in Commands.
type Mock struct {
	mu        sync.Mutex
	Name      string
	Commands  []string
	Stdins    []string
	Responses []MockResponse
	Lines     []string // emitted by Stream
	DialFunc  func(ctx context.Context, network, addr string) (net.Conn, error)
}

type MockResponse struct {
	Contains string
	Out      string
	Err      error
	// Times limits how often this response matches (0 = unlimited).
	Times int
	used  int
}

func (m *Mock) String() string { return "mock://" + m.Name }

func (m *Mock) On(contains, out string, err error) *Mock {
	m.Responses = append(m.Responses, MockResponse{Contains: contains, Out: out, Err: err})
	return m
}

func (m *Mock) Run(ctx context.Context, cmd string, stdin io.Reader) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Commands = append(m.Commands, cmd)
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		m.Stdins = append(m.Stdins, string(b))
	}
	for i := range m.Responses {
		r := &m.Responses[i]
		if strings.Contains(cmd, r.Contains) && (r.Times == 0 || r.used < r.Times) {
			r.used++
			return r.Out, r.Err
		}
	}
	return "", nil
}

func (m *Mock) Stream(ctx context.Context, cmd string, onLine func(string)) error {
	m.mu.Lock()
	m.Commands = append(m.Commands, cmd)
	lines := append([]string(nil), m.Lines...)
	m.mu.Unlock()
	for _, l := range lines {
		onLine(l)
	}
	<-ctx.Done()
	return ctx.Err()
}

func (m *Mock) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if m.DialFunc != nil {
		return m.DialFunc(ctx, network, addr)
	}
	return nil, fmt.Errorf("mock: dial not configured")
}

// Joined returns all recorded commands, newline separated.
func (m *Mock) Joined() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.Commands, "\n")
}
