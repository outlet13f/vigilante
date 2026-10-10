package main

import (
	"context"
	"os"
	"time"

	"vigilante/internal/api"
	"vigilante/internal/ha"
	"vigilante/internal/orchestrator"
	"vigilante/internal/tlsconf"
)

// startHA makes this server one of several nodes sharing a postgres store.
// The node starts passive and fenced (its writes are refused unless it holds
// the leader lease); when elected it reloads the shared state, starts acting
// and resumes any rollback the previous leader left in flight.
func startHA(ctx context.Context, e *orchestrator.Engine, srv *api.Server) (*ha.Elector, error) {
	cfg := e.Cfg.Server.HA
	forwardTLS, err := tlsconf.HAClient(cfg.TLS)
	if err != nil {
		return nil, err
	}
	srv.HATLS = forwardTLS
	node := cfg.NodeID
	if node == "" {
		node, _ = os.Hostname()
	}
	el := &ha.Elector{Store: e.Journal, NodeID: node, Advertise: cfg.AdvertiseURL, TTL: cfg.LeaseTTL, Log: e.Log}
	el.OnElected = func(lctx context.Context) {
		for {
			if err := e.Reload(lctx); err == nil {
				break
			} else {
				e.Log.Error("new leader could not load shared state; retrying", "err", err)
			}
			select {
			case <-lctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
		e.SetActive(true)
		if n := e.Resume(lctx); n > 0 {
			e.Log.Warn("resumed rollbacks left in flight by the previous leader", "count", n)
		}
	}
	el.OnDemoted = func() { e.SetActive(false) }
	e.SetActive(false)
	e.Journal.Fence(ha.LeaderKey, el.Owner())
	srv.HA = el
	go el.Run(ctx)
	return el, nil
}
