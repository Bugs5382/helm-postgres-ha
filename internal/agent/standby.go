package agent

/*
MIT License

Copyright (c) 2026 Shane

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
*/

import (
	"context"
	"fmt"
	"time"

	golog "github.com/Bugs5382/go-log"

	"github.com/Bugs5382/helm-postgres-ha/internal/errs"
	"github.com/Bugs5382/helm-postgres-ha/internal/kube"
	"github.com/Bugs5382/helm-postgres-ha/internal/lease"
	"github.com/Bugs5382/helm-postgres-ha/internal/peer"
	"github.com/Bugs5382/helm-postgres-ha/internal/pg"
)

// follow runs while another member holds a live Lease: this member is its
// standby.
func (a *Agent) follow(ctx context.Context, rec lease.Record, l local) {
	holder := rec.Holder
	a.m.LeaseHolder.Set(0)
	a.primarySince = time.Time{}
	if l.running && l.up && !l.st.InRecovery {
		a.fence(ctx, fmt.Sprintf("running read-write while %s holds the lease", holder))
		return
	}
	if l.running && !l.up {
		if l.lastWasRW {
			a.fence(ctx, fmt.Sprintf("starting read-write while %s holds the lease", holder))
			return
		}
		a.wait("postgres is starting")
		return
	}
	a.holdingRW.Store(false)
	if !l.running {
		if l.lastWasRW || l.rejoin {
			a.startTask("rejoin", func(ctx context.Context) error { return a.rejoin(ctx, holder) })
			return
		}
		_ = a.node.SetSignal(pg.StandbySignal, true)
		if _, err := a.node.WriteAgentConf(a.standbySettings(holder)); err != nil {
			a.log.Error(err, "cannot write agent settings")
			return
		}
		a.node.CloseConns()
		if err := a.node.Start(); err != nil {
			a.log.Error(err, "cannot start postgres")
			return
		}
		a.following, a.notStreaming = holder, a.now()
		a.log.Info("started as a standby", golog.F("upstream", holder))
		return
	}
	if a.following != holder {
		changed, err := a.node.WriteAgentConf(a.standbySettings(holder))
		if err != nil {
			a.log.Error(err, "cannot write agent settings")
			return
		}
		if changed {
			if err := a.node.Reload(); err != nil {
				a.log.Warn("cannot reload postgres", golog.F("error", err.Error()))
				return
			}
		}
		a.log.Info("following the primary", golog.F("upstream", holder), golog.F("previous", a.following))
		a.following, a.notStreaming = holder, a.now()
	}
	a.setRole(ctx, kube.RoleReplica)
	if l.st.Streaming() {
		a.notStreaming = time.Time{}
		a.lastRejoinNoop = false
		return
	}
	if a.notStreaming.IsZero() {
		a.notStreaming = a.now()
	}
	if stuck := a.now().Sub(a.notStreaming); stuck > a.cfg.RejoinTimeout {
		// The primary is up but this standby cannot stream from it: its
		// timeline diverged or the WAL it needs is gone. Rewind, or re-clone.
		if !a.holderIsPrimary(ctx, holder) {
			a.wait("not streaming, and the primary is not answering yet")
			return
		}
		a.log.Warn("not streaming from a healthy primary; rejoining", golog.F("upstream", holder), golog.F("stuck_ms", stuck.Milliseconds()))
		_ = a.node.SetSignal(RejoinMarker, true)
		sctx, cancel := context.WithTimeout(ctx, a.cfg.ShutdownTimeout)
		defer cancel()
		_ = a.node.Stop(sctx, pg.StopFast)
		a.node.CloseConns()
		a.notStreaming = time.Time{}
	}
}

func (a *Agent) holderIsPrimary(ctx context.Context, holder string) bool {
	r := a.peers.FetchAll(ctx, []string{holder})[holder]
	return r.Err == nil && r.Status.Role == peer.RolePrimary && r.Status.Running && !r.Status.InRecovery
}

// rejoin brings data that may have diverged back in line with the primary:
// pg_rewind first, and when that fails, the data is moved aside (never
// deleted) and the member is cloned again.
func (a *Agent) rejoin(ctx context.Context, holder string) error {
	if !a.holderIsPrimary(ctx, holder) {
		return fmt.Errorf("waiting for %s to run as primary before rejoining", holder)
	}
	a.setRole(ctx, kube.RoleNone)
	if a.lastRejoinNoop {
		// Rewinding found nothing to change last time and the standby still
		// could not stream: the WAL it needs is gone. Only a new copy helps.
		a.lastRejoinNoop = false
		a.log.Warn("still not streaming after a no-op rewind; moving the data aside and cloning again", golog.F("upstream", holder))
		aside, err := a.node.MoveAside("fell-behind")
		if err != nil {
			return fmt.Errorf("move data aside: %w", err)
		}
		a.log.Warn("old data kept", golog.F("path", aside))
		if err := a.clone(ctx, holder); err != nil {
			return err
		}
		return a.node.SetSignal(RejoinMarker, false)
	}
	changed, err := a.node.Rewind(ctx, a.conninfo(holder, pg.RoleRewind, "postgres"))
	if err != nil {
		a.m.Rewinds.WithLabelValues("failed").Inc()
		a.logCoded(errs.New(errs.Rewind, err), "rewind failed; moving the data aside and cloning again")
		aside, merr := a.node.MoveAside("rewind-failed")
		if merr != nil {
			return fmt.Errorf("move data aside: %w", merr)
		}
		a.log.Warn("old data kept", golog.F("path", aside))
		if err := a.clone(ctx, holder); err != nil {
			return err
		}
	} else {
		outcome := "noop"
		if changed {
			outcome = "rewound"
		}
		a.lastRejoinNoop = !changed
		a.m.Rewinds.WithLabelValues(outcome).Inc()
		a.log.Info("rewind finished", golog.F("outcome", outcome), golog.F("upstream", holder))
	}
	if err := a.node.SetSignal(pg.StandbySignal, true); err != nil {
		return err
	}
	if _, err := a.node.WriteAgentConf(a.standbySettings(holder)); err != nil {
		return err
	}
	return a.node.SetSignal(RejoinMarker, false)
}

// clone copies the primary into the (empty) data directory.
func (a *Agent) clone(ctx context.Context, holder string) error {
	if err := a.node.Clone(ctx, a.conninfo(holder, pg.RoleReplication, "")); err != nil {
		a.m.Clones.WithLabelValues("failed").Inc()
		return errs.New(errs.Clone, err)
	}
	a.m.Clones.WithLabelValues("ok").Inc()
	if _, err := a.node.WriteAgentConf(a.standbySettings(holder)); err != nil {
		return err
	}
	return a.node.SetSignal(pg.StandbySignal, true)
}
