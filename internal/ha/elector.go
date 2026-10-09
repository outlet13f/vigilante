// Package ha elects one leader among `vigilante server` nodes that share a
// postgres state store. The leader holds a lease row that it renews every
// TTL/3; a follower takes over once the lease expires. The lease owner string
// carries the leader's advertise URL so followers can forward API calls.
package ha

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"vigilante/internal/store"
)

// LeaderKey is the lease that decides leadership.
const LeaderKey = "leader"

type Elector struct {
	Store     store.Store
	NodeID    string
	Advertise string // this node's API base URL
	TTL       time.Duration
	Log       *slog.Logger
	// OnElected runs when this node becomes leader; its context is cancelled
	// on demotion. OnDemoted runs after leadership is lost.
	OnElected func(ctx context.Context)
	OnDemoted func()

	mu         sync.RWMutex
	leader     bool
	leaderAddr string
}

// Owner encodes "node-id|advertise-url" into the lease.
func (e *Elector) Owner() string { return e.NodeID + "|" + e.Advertise }

func parseOwner(o string) (node, addr string) {
	node, addr, _ = strings.Cut(o, "|")
	return
}

// IsLeader reports whether this node currently leads.
func (e *Elector) IsLeader() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.leader
}

// Leader returns the current leader's node ID and URL ("" when unknown).
func (e *Elector) Leader() (node, addr string) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.leader {
		return e.NodeID, e.Advertise
	}
	return parseOwner(e.leaderAddr)
}

// Run campaigns until ctx ends, then releases the lease if held.
func (e *Elector) Run(ctx context.Context) {
	interval := e.TTL / 3
	var cancelLeader context.CancelFunc
	var lastOK time.Time
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		e.step(ctx, &cancelLeader, &lastOK)
		select {
		case <-ctx.Done():
			if e.IsLeader() {
				e.demote(&cancelLeader)
				rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = e.Store.ReleaseLease(rctx, LeaderKey, e.Owner())
				cancel()
			}
			return
		case <-tick.C:
		}
	}
}

func (e *Elector) step(ctx context.Context, cancelLeader *context.CancelFunc, lastOK *time.Time) {
	cctx, cancel := context.WithTimeout(ctx, e.TTL/3)
	ok, err := e.Store.TryLease(cctx, LeaderKey, e.Owner(), e.TTL)
	cancel()
	switch {
	case err == nil && ok:
		*lastOK = time.Now()
		if !e.IsLeader() {
			e.mu.Lock()
			e.leader, e.leaderAddr = true, e.Owner()
			e.mu.Unlock()
			e.Store.Fence(LeaderKey, e.Owner())
			e.Log.Info("elected leader", "node", e.NodeID)
			lctx, lcancel := context.WithCancel(ctx)
			*cancelLeader = lcancel
			if e.OnElected != nil {
				go e.OnElected(lctx)
			}
		}
	case err == nil && !ok:
		if e.IsLeader() { // someone else holds it: we were demoted
			e.demote(cancelLeader)
		}
		holder, _ := e.Store.LeaseHolder(ctx, LeaderKey)
		e.mu.Lock()
		e.leaderAddr = holder
		e.mu.Unlock()
	default:
		// Store unreachable. Keep leading only while our lease can still be
		// valid; past the TTL another node may already lead.
		e.Log.Warn("leader lease renewal failed", "err", err)
		if e.IsLeader() && time.Since(*lastOK) > e.TTL {
			e.demote(cancelLeader)
		}
	}
}

func (e *Elector) demote(cancelLeader *context.CancelFunc) {
	e.mu.Lock()
	e.leader = false
	e.mu.Unlock()
	if *cancelLeader != nil {
		(*cancelLeader)()
		*cancelLeader = nil
	}
	e.Log.Warn("lost leadership", "node", e.NodeID)
	if e.OnDemoted != nil {
		e.OnDemoted()
	}
}
