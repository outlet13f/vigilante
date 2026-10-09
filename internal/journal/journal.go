// Package journal is an append-only JSONL write-ahead log of everything the
// orchestrator decides and does. It doubles as the audit trail and as the
// recovery source: after a crash, Replay rebuilds deployment state, the
// circuit breaker and rollback history, and reports rollbacks that were in
// flight so they can be resumed (rollback steps are idempotent).
package journal

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"vigilante/internal/model"
	"vigilante/internal/safety"
)

const (
	KindDeployment    = "deployment"     // full deployment snapshot
	KindCircuit       = "circuit"        // circuit breaker state
	KindRollbackStart = "rollback.start" // service rollback begins (flapping history)
	KindStepDone      = "rollback.step"  // target finished plan step N
	KindAudit         = "audit"          // free-form action audit line
)

type Entry struct {
	Time       time.Time            `json:"time"`
	Kind       string               `json:"kind"`
	Service    string               `json:"service,omitempty"`
	Deployment *model.Deployment    `json:"deployment,omitempty"`
	Circuit    *safety.CircuitState `json:"circuit,omitempty"`
	DeployID   string               `json:"deployment_id,omitempty"`
	Target     string               `json:"target,omitempty"`
	Step       int                  `json:"step,omitempty"`
	Message    string               `json:"message,omitempty"`
}

type Journal struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

func Open(path string) (*Journal, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Journal{f: f, path: path}, nil
}

func (j *Journal) Path() string { return j.path }

// Append writes and fsyncs one entry; decisions are durable before actions run.
func (j *Journal) Append(e Entry) error {
	if j == nil {
		return nil
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.f.Write(append(b, '\n')); err != nil {
		return err
	}
	return j.f.Sync()
}

func (j *Journal) Close() error {
	if j == nil {
		return nil
	}
	return j.f.Close()
}

// State is the result of replaying a journal.
type State struct {
	Deployments map[string]*model.Deployment
	Circuit     *safety.CircuitState
	Rollbacks   map[string][]time.Time    // service -> rollback start times
	StepsDone   map[string]map[string]int // deployment -> target -> highest completed step index + 1
	Corrupt     int                       // unparsable lines skipped (e.g. torn final write)
}

// InFlight returns deployments whose rollback had started but not finished.
func (s *State) InFlight() []*model.Deployment {
	var out []*model.Deployment
	for _, d := range s.Deployments {
		if d.State == model.StateRollingBack {
			out = append(out, d)
		}
	}
	return out
}

func Replay(path string) (*State, error) {
	st := &State{
		Deployments: map[string]*model.Deployment{},
		Rollbacks:   map[string][]time.Time{},
		StepsDone:   map[string]map[string]int{},
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			st.Corrupt++
			continue
		}
		switch e.Kind {
		case KindDeployment:
			if e.Deployment != nil {
				st.Deployments[e.Deployment.ID] = e.Deployment
			}
		case KindCircuit:
			st.Circuit = e.Circuit
		case KindRollbackStart:
			st.Rollbacks[e.Service] = append(st.Rollbacks[e.Service], e.Time)
		case KindStepDone:
			m := st.StepsDone[e.DeployID]
			if m == nil {
				m = map[string]int{}
				st.StepsDone[e.DeployID] = m
			}
			if e.Step+1 > m[e.Target] {
				m[e.Target] = e.Step + 1
			}
		}
	}
	if err := sc.Err(); err != nil {
		return st, fmt.Errorf("replay %s: %w", path, err)
	}
	return st, nil
}
