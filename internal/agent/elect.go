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
	"errors"
	"fmt"
	"strconv"
	"time"

	golog "github.com/Bugs5382/go-log"

	"github.com/Bugs5382/helm-postgres-ha/internal/election"
	"github.com/Bugs5382/helm-postgres-ha/internal/errs"
	"github.com/Bugs5382/helm-postgres-ha/internal/lease"
	"github.com/Bugs5382/helm-postgres-ha/internal/peer"
	"github.com/Bugs5382/helm-postgres-ha/internal/pg"
)

// elect runs when the Lease has no live holder and this member has data.
func (a *Agent) elect(ctx context.Context, rec lease.Record, l local) {
	a.m.LeaseHolder.Set(0)
	a.primarySince = time.Time{}
	if l.running && l.up && !l.st.InRecovery {
		a.fence(ctx, "running read-write without the lease")
		return
	}
	if l.running && !l.up {
		if l.lastWasRW {
			a.fence(ctx, "starting read-write without the lease")
			return
		}
		a.wait("postgres is starting")
		return
	}
	if !l.running {
		// Start as a standby with no upstream: it replays its own WAL and can
		// report its position. Data that last ran read-write is marked for a
		// rewind in case another member wins.
		if l.lastWasRW {
			_ = a.node.SetSignal(RejoinMarker, true)
		}
		_ = a.node.SetSignal(pg.StandbySignal, true)
		if _, err := a.node.WriteAgentConf(a.standbySettings("")); err != nil {
			a.log.Error(err, "cannot write agent settings")
			return
		}
		a.node.CloseConns()
		if err := a.node.Start(); err != nil {
			a.log.Error(err, "cannot start postgres")
		}
		a.following = ""
		return
	}
	if a.following != "" {
		// The holder is gone: stop streaming settings pointing at it so the
		// status reports this member's own position.
		if _, err := a.node.WriteAgentConf(a.standbySettings("")); err == nil {
			_ = a.node.Reload()
		}
		a.following = ""
	}
	res := a.peers.FetchAll(ctx, a.others())
	res[a.me] = peer.Result{Status: a.selfStatus(l, rec)}
	a.countReachable(res)
	in := a.electionInput(rec, res)
	d := election.Failover(in)
	if !d.Acquire {
		a.wait("no failover: " + d.Reason)
		return
	}
	a.log.Info("taking the lease", golog.F("reason", d.Reason), golog.F("previous_holder", rec.Holder), golog.F("timeline", l.st.Timeline), golog.F("position", l.st.Position().String()))
	if _, err := a.acquire(ctx, rec, nil); err != nil {
		if errors.Is(err, lease.ErrConflict) {
			a.log.Info("another member took the lease first")
		} else {
			a.logCoded(errs.New(errs.LeaseUnreadable, err), "cannot take the lease")
		}
	}
}

func (a *Agent) acquire(ctx context.Context, rec lease.Record, extra map[string]string) (lease.Record, error) {
	ann := map[string]string{lease.Switchover: "", lease.Successor: "", lease.SuccessorAt: ""}
	if a.cfg.Revision != "" {
		ann[lease.Revision] = a.cfg.Revision
	}
	for k, v := range extra {
		ann[k] = v
	}
	actx, cancel := context.WithTimeout(ctx, a.cfg.PeerTimeout)
	defer cancel()
	sent := a.now()
	nr, err := a.lease.Acquire(actx, rec, a.me, ann)
	if err != nil {
		return rec, err
	}
	a.lastRenew.Store(sent.UnixNano())
	a.log.Info("lease acquired", golog.F("transitions", nr.Transitions))
	return nr, nil
}

func (a *Agent) countReachable(res map[string]peer.Result) {
	n := 0
	for _, r := range res {
		if r.Err == nil {
			n++
		}
	}
	a.m.PeersReachable.Set(float64(n))
	for m, r := range res {
		if r.Err != nil {
			golog.Trace(a.log, "member unreachable", golog.F("member", m), golog.F("error", r.Err.Error()))
		}
	}
}

func (a *Agent) electionInput(rec lease.Record, res map[string]peer.Result) election.Input {
	in := election.Input{
		Self:        a.me,
		Topology:    a.topo,
		Members:     res,
		SystemID:    rec.Ann(lease.SystemID),
		MaxLag:      a.cfg.MaxLagOnFailover,
		OldRevision: rec.Ann(lease.Revision),
	}
	in.LastTimeline, _ = strconv.Atoi(rec.Ann(lease.Timeline))
	in.LastLSN, _ = strconv.ParseUint(rec.Ann(lease.LSN), 10, 64)
	if s := rec.Ann(lease.Successor); s != "" && rec.Holder == "" && a.lease.ObservedFor() < 2*a.cfg.LeaseDuration {
		in.Successor, in.SuccessorFresh = s, true
	}
	return in
}

// noData runs when this member's data directory is empty.
func (a *Agent) noData(ctx context.Context, rec lease.Record, l local) {
	a.holdingRW.Store(false)
	a.m.LeaseHolder.Set(0)
	sysid := rec.Ann(lease.SystemID)
	live := rec.Holder != "" && !a.lease.Expired(rec)
	switch {
	case rec.Holder == a.me && live && sysid != "":
		// The data went away while this member held the Lease (a replaced
		// volume). Never bootstrap over a cluster that exists: hand the
		// Lease back so a member with data takes over.
		a.log.Error(errors.New("data directory is empty"), "holding the lease of an existing cluster without data; releasing it")
		_, _ = a.lease.Release(ctx, rec, a.me, nil)
	case rec.Holder == a.me && live:
		a.startBootstrap()
	case live:
		holder := rec.Holder
		a.startTask("clone", func(ctx context.Context) error {
			if !a.holderIsPrimary(ctx, holder) {
				return fmt.Errorf("waiting for %s to run as primary before cloning", holder)
			}
			return a.clone(ctx, holder)
		})
	case sysid == "":
		res := a.peers.FetchAll(ctx, a.others())
		res[a.me] = peer.Result{Status: a.selfStatus(l, rec)}
		a.countReachable(res)
		d := election.Bootstrap(a.electionInput(rec, res))
		if !d.Acquire {
			a.wait("bootstrap: " + d.Reason)
			return
		}
		a.log.Info("bootstrapping the cluster", golog.F("mode", a.cfg.Bootstrap.Mode))
		if _, err := a.acquire(ctx, rec, nil); err != nil {
			a.log.Info("could not take the lease to bootstrap", golog.F("error", err.Error()))
			return
		}
		a.startBootstrap()
	default:
		a.wait("the cluster has data elsewhere; waiting for a primary to clone from")
	}
}

// startBootstrap creates the cluster's first data in the background while the
// loop keeps the Lease.
func (a *Agent) startBootstrap() {
	a.bootstrapped = true
	if a.restoring() {
		a.startTask("restore", func(ctx context.Context) error {
			r := a.cfg.Bootstrap.Restore
			// Restoring from the cluster's own prefix continues its history;
			// a restore from elsewhere starts a new one there.
			if r.Prefix != "" {
				if err := a.checkArchiveUnused(ctx); err != nil {
					return err
				}
			}
			if err := a.node.FetchBackup(ctx, r.Prefix, r.Backup); err != nil {
				return errs.New(errs.Bootstrap, fmt.Errorf("fetch backup %s: %w", r.Backup, err))
			}
			if _, err := a.node.WriteAgentConf(a.restoreSettings()); err != nil {
				return err
			}
			return a.node.SetSignal(pg.RecoverySignal, true)
		})
		return
	}
	a.startTask("initdb", func(ctx context.Context) error {
		if err := a.checkArchiveUnused(ctx); err != nil {
			return err
		}
		if err := a.node.Initdb(ctx); err != nil {
			return errs.New(errs.Bootstrap, err)
		}
		_, err := a.node.WriteAgentConf(a.primarySettings())
		return err
	})
}

// checkArchiveUnused refuses a new cluster's first data while its backup
// storage holds another cluster's backups or WAL. Without backups nothing is
// archived, so there is nothing to check.
func (a *Agent) checkArchiveUnused(ctx context.Context) error {
	if !a.cfg.Backup.Enabled {
		return nil
	}
	used, err := a.node.ArchiveUsed(ctx)
	if err != nil {
		return errs.New(errs.Bootstrap, fmt.Errorf("check the backup storage before archiving into it: %w", err))
	}
	if used {
		return errs.New(errs.ArchiveInUse, errors.New("the backup storage prefix already holds backups or WAL from another cluster; set backup.s3.prefix to an empty prefix, or empty this one"))
	}
	a.log.Info("backup storage is empty; bootstrapping")
	return nil
}
