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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	golog "github.com/Bugs5382/go-log"

	"github.com/Bugs5382/helm-postgres-ha/internal/cluster"
	"github.com/Bugs5382/helm-postgres-ha/internal/election"
	"github.com/Bugs5382/helm-postgres-ha/internal/errs"
	"github.com/Bugs5382/helm-postgres-ha/internal/kube"
	"github.com/Bugs5382/helm-postgres-ha/internal/lease"
	"github.com/Bugs5382/helm-postgres-ha/internal/pg"
)

// dutyEvery is how often a primary re-checks its Service, slots, roles and
// statistics.
const dutyEvery = 30 * time.Second

// renew extends the Lease. On a read-write primary it also records the
// position a failover candidate must be close to.
func (a *Agent) renew(ctx context.Context, rec lease.Record, l local) (lease.Record, error) {
	ann := map[string]string{lease.Successor: "", lease.SuccessorAt: ""}
	if a.cfg.Revision != "" {
		ann[lease.Revision] = a.cfg.Revision
	}
	if l.up && !l.st.InRecovery {
		ann[lease.Timeline] = strconv.Itoa(l.st.Timeline)
		ann[lease.LSN] = strconv.FormatUint(uint64(l.st.CurrentLSN), 10)
		if rec.Ann(lease.SystemID) == "" && l.st.SystemID != "" {
			ann[lease.SystemID] = l.st.SystemID
			a.log.Info("recording the cluster system identifier", golog.F("system_identifier", l.st.SystemID))
		}
	}
	for k, v := range ann {
		if rec.Ann(k) == v {
			delete(ann, k)
		}
	}
	rctx, cancel := context.WithTimeout(ctx, a.cfg.PeerTimeout)
	defer cancel()
	sent := a.now()
	nr, err := a.lease.Renew(rctx, rec, a.me, ann)
	if err != nil {
		a.log.Warn("lease renew failed", golog.F("error", err.Error()), golog.F("since_last_renew_ms", sent.Sub(time.Unix(0, a.lastRenew.Load())).Milliseconds()))
		return rec, err
	}
	a.lastRenew.Store(sent.UnixNano())
	golog.Trace(a.log, "lease renewed")
	return nr, nil
}

// lead runs while this member holds the Lease.
func (a *Agent) lead(ctx context.Context, rec lease.Record, l local) {
	rec, err := a.renew(ctx, rec, l)
	if err != nil {
		// The watchdog fences if this persists past the renew deadline.
		return
	}
	a.m.LeaseHolder.Set(1)
	if !l.running {
		a.startAsHolder(l)
		return
	}
	if !l.up {
		a.wait("postgres is starting")
		return
	}
	if l.st.InRecovery {
		if l.recovery {
			a.wait("restoring from backup; postgres promotes itself at the recovery target")
			return
		}
		a.promote(rec)
		return
	}
	a.holdingRW.Store(true)
	if l.recovery || l.rejoin {
		_ = a.node.SetSignal(pg.RecoverySignal, false)
		_ = a.node.SetSignal(RejoinMarker, false)
	}
	a.primaryDuties(ctx, rec, l)
	if target := rec.Ann(lease.Switchover); target != "" {
		a.handover(ctx, rec, target, false)
	}
}

// startAsHolder starts PostgreSQL on a member that holds the Lease. Data
// that last ran read-write starts read-write; a standby's data starts as a
// standby and is promoted on the next round.
func (a *Agent) startAsHolder(l local) {
	switch {
	case l.recovery:
		// Restore: the settings were written with the backup.
	case l.lastWasRW && !l.rejoin:
		if _, err := a.node.WriteAgentConf(a.primarySettings()); err != nil {
			a.log.Error(err, "cannot write agent settings")
			return
		}
		a.holdingRW.Store(true)
	default:
		_ = a.node.SetSignal(pg.StandbySignal, true)
		if _, err := a.node.WriteAgentConf(a.standbySettings("")); err != nil {
			a.log.Error(err, "cannot write agent settings")
			return
		}
	}
	a.node.CloseConns()
	if err := a.node.Start(); err != nil {
		a.holdingRW.Store(false)
		a.log.Error(err, "cannot start postgres")
		return
	}
	a.primarySince = time.Time{}
}

// promote ends recovery in the background while the loop keeps renewing.
func (a *Agent) promote(rec lease.Record) {
	a.holdingRW.Store(true)
	a.log.Info("promoting to primary", golog.F("timeout_ms", a.cfg.PromoteTimeout.Milliseconds()))
	a.startTask("promote", func(ctx context.Context) error {
		pctx, cancel := context.WithTimeout(ctx, a.cfg.PromoteTimeout+5*time.Second)
		defer cancel()
		ok, err := a.node.Promote(pctx, a.cfg.PromoteTimeout)
		if err == nil && ok {
			a.m.Promotions.Inc()
			_ = a.node.SetSignal(RejoinMarker, false)
			a.log.Info("promotion finished")
			return nil
		}
		sctx, scancel := context.WithTimeout(ctx, a.cfg.PeerTimeout)
		defer scancel()
		if st, serr := a.node.Status(sctx); serr == nil && !st.InRecovery {
			a.m.Promotions.Inc()
			a.log.Info("promotion finished after the wait")
			return nil
		}
		// Still a standby: give the Lease up so another member can try,
		// and never point clients at a server that cannot take writes.
		a.holdingRW.Store(false)
		a.m.PromoteFailures.Inc()
		if err == nil {
			err = fmt.Errorf("pg_promote did not finish within %s", a.cfg.PromoteTimeout)
		}
		cur, gerr := a.lease.Get(ctx)
		if gerr == nil && cur.Holder == a.me {
			if _, rerr := a.lease.Release(ctx, cur, a.me, nil); rerr != nil {
				a.log.Warn("cannot release the lease after a failed promotion", golog.F("error", rerr.Error()))
			}
		}
		return errs.New(errs.Promote, err)
	})
}

// primaryDuties keeps a read-write primary's surroundings right: the role
// label the primary Service selects, replication slots and roles.
func (a *Agent) primaryDuties(ctx context.Context, rec lease.Record, l local) {
	first := a.primarySince.IsZero()
	if first {
		a.syncNames = a.desiredSync(nil)
		// Drop any restore-only settings now that this server is the primary.
		if changed, err := a.node.WriteAgentConf(a.primarySettings()); err != nil {
			a.log.Error(err, "cannot write agent settings")
		} else if changed {
			_ = a.node.Reload()
		}
		a.primarySince = a.now()
		a.labelled = ""
		a.rolesMu.Lock()
		a.rolesStamp = ""
		a.rolesMu.Unlock()
		a.log.Info("serving as primary", golog.F("timeline", l.st.Timeline), golog.F("lsn", l.st.CurrentLSN.String()))
	}
	if first {
		a.clearOtherPrimaries(ctx)
		a.resetPoolers()
	}
	a.setRole(ctx, kube.RolePrimary)
	a.updateSync(ctx)
	if first || a.now().Sub(a.lastDuty) >= dutyEvery {
		a.lastDuty = a.now()
		dctx, cancel := context.WithTimeout(ctx, 2*a.cfg.PeerTimeout)
		a.ensureSlots(dctx)
		a.checkLabels(dctx)
		a.checkPeersForSplitBrain(dctx)
		a.collectPrimaryStats(dctx)
		cancel()
	}
	a.ensureRoles()
	if first && a.bk != nil && (a.bootstrapped || rec.Ann(lease.SystemID) == "") {
		a.bk.RequestNow()
		a.bootstrapped = false
	}
}

// resetPoolers drops PgBouncer's connections to the old primary, in the
// background so a dead pooler pod cannot hold up the loop.
func (a *Agent) resetPoolers() {
	if a.poolers == nil {
		return
	}
	a.bg.Add(1)
	go func() {
		defer a.bg.Done()
		ctx, cancel := context.WithTimeout(a.runCtx, 15*time.Second)
		defer cancel()
		start := a.now()
		n, err := a.poolers.Reset(ctx)
		if err != nil {
			a.log.Warn("some poolers could not be reset", golog.F("reset", n), golog.F("error", err.Error()))
		}
		a.log.Info("poolers reset after the promotion", golog.F("reset", n), golog.F("elapsed_ms", a.now().Sub(start).Milliseconds()))
	}()
}

// clearOtherPrimaries takes the primary label off every other member, so the
// primary Service never selects a former primary that could not relabel
// itself (a partitioned node, a crashed agent).
func (a *Agent) clearOtherPrimaries(ctx context.Context) {
	for _, m := range a.others() {
		role, err := a.kube.Role(ctx, m)
		if err != nil || role != kube.RolePrimary {
			continue
		}
		if err := a.kube.SetRole(ctx, m, kube.RoleNone); err != nil {
			a.log.Warn("cannot clear a former primary's label", golog.F("member", m), golog.F("error", err.Error()))
			continue
		}
		a.log.Info("cleared the primary label of a former primary", golog.F("member", m))
	}
}

// checkLabels corrects label drift: this member's label is re-applied and no
// other member may carry the primary label.
func (a *Agent) checkLabels(ctx context.Context) {
	if role, err := a.kube.Role(ctx, a.me); err == nil && role != kube.RolePrimary {
		a.log.Warn("primary label missing; restoring it", golog.F("label", role))
		a.labelled = ""
		a.setRole(ctx, kube.RolePrimary)
	}
	a.clearOtherPrimaries(ctx)
}

// ensureSlots keeps one physical slot per other member and drops slots this
// cluster created for members that no longer exist.
func (a *Agent) ensureSlots(ctx context.Context) {
	slots, err := a.node.Slots(ctx)
	if err != nil {
		a.log.Warn("cannot list replication slots", golog.F("error", err.Error()))
		return
	}
	want := map[string]bool{}
	for _, m := range a.topo.Members() {
		if m != a.me {
			want[cluster.SlotName(m)] = true
		}
	}
	have := map[string]bool{}
	for _, s := range slots {
		have[s.Name] = true
		if !want[s.Name] && strings.HasPrefix(s.Name, a.topo.SlotPrefix()) && !s.Active {
			if err := a.node.DropSlot(ctx, s.Name); err != nil {
				a.log.Warn("cannot drop replication slot", golog.F("slot", s.Name), golog.F("error", err.Error()))
			} else {
				a.log.Info("dropped replication slot", golog.F("slot", s.Name))
			}
		}
	}
	names := make([]string, 0, len(want))
	for n := range want {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if have[n] {
			continue
		}
		if err := a.node.CreateSlot(ctx, n); err != nil {
			a.log.Warn("cannot create replication slot", golog.F("slot", n), golog.F("error", err.Error()))
		} else {
			a.log.Info("created replication slot", golog.F("slot", n))
		}
	}
}

// updateSync keeps synchronous_standby_names in step with the standbys that
// stream right now.
func (a *Agent) updateSync(ctx context.Context) {
	qctx, cancel := context.WithTimeout(ctx, a.cfg.PeerTimeout)
	defer cancel()
	reps, err := a.node.Replicas(qctx)
	if err != nil {
		golog.Trace(a.log, "cannot read pg_stat_replication", golog.F("error", err.Error()))
		return
	}
	var streaming []string
	for _, r := range reps {
		if r.State == "streaming" && a.topo.IsMember(r.Name) && r.Name != a.me {
			streaming = append(streaming, r.Name)
		}
	}
	want := a.desiredSync(streaming)
	if want == a.syncNames {
		return
	}
	prev := a.syncNames
	a.syncNames = want
	changed, err := a.node.WriteAgentConf(a.primarySettings())
	if err != nil {
		a.log.Error(err, "cannot write agent settings")
		return
	}
	if changed {
		_ = a.node.Reload()
	}
	a.log.Info("synchronous standbys changed", golog.F("synchronous_standby_names", want), golog.F("previous", prev))
}

// ensureRoles applies the role and database spec when it, or a password,
// changed since the last successful run. It runs off the loop: role and
// database DDL can take a while and must never delay a lease renew.
func (a *Agent) ensureRoles() {
	if a.rolesBusy.Load() {
		return
	}
	stamp := a.stamp(a.cfg.RolesFile())
	for _, s := range []string{"superuser", "replication", "rewind", "pgbouncer"} {
		stamp += a.stamp(a.cfg.SecretFile(s))
	}
	spec, err := a.spec(a.cfg.RolesFile())
	if err != nil {
		a.logCoded(errs.New(errs.Roles, err), "cannot read the role spec")
		return
	}
	for _, r := range spec.Roles {
		stamp += a.stamp(r.PasswordFile)
	}
	a.rolesMu.Lock()
	same := stamp == a.rolesStamp
	a.rolesMu.Unlock()
	if same {
		return
	}
	creds, err := a.credentials()
	if err != nil {
		a.logCoded(errs.New(errs.Roles, err), "cannot read credentials")
		return
	}
	a.rolesBusy.Store(true)
	a.bg.Add(1)
	go func() {
		defer a.bg.Done()
		defer a.rolesBusy.Store(false)
		ctx, cancel := context.WithTimeout(a.runCtx, 2*time.Minute)
		defer cancel()
		start := a.now()
		if err := a.node.ApplyRoles(ctx, spec, creds); err != nil {
			a.logCoded(errs.New(errs.Roles, err), "cannot apply roles and databases")
			return
		}
		a.rolesMu.Lock()
		a.rolesStamp = stamp
		a.rolesMu.Unlock()
		a.log.Info("roles and databases applied", golog.F("roles", len(spec.Roles)), golog.F("databases", len(spec.Databases)), golog.F("elapsed_ms", a.now().Sub(start).Milliseconds()))
	}()
}

func (a *Agent) others() []string {
	var out []string
	for _, m := range a.topo.Members() {
		if m != a.me {
			out = append(out, m)
		}
	}
	return out
}

// checkPeersForSplitBrain looks for another member running read-write. It
// only reports: that member fences itself because it cannot hold the Lease.
func (a *Agent) checkPeersForSplitBrain(ctx context.Context) {
	res := a.peers.FetchAll(ctx, a.others())
	split := false
	for m, r := range res {
		if r.Err == nil && r.Status.Running && !r.Status.InRecovery && r.Status.HasData {
			split = true
			a.log.Error(errors.New("split brain"), "another member reports running read-write", golog.F("member", m))
		}
	}
	a.split = split
	if split {
		a.m.SplitBrain.Set(1)
	} else {
		a.m.SplitBrain.Set(0)
	}
}

func (a *Agent) collectPrimaryStats(ctx context.Context) {
	if arc, err := a.node.Archiver(ctx); err == nil {
		a.m.ArchiveArchived.Set(float64(arc.Archived))
		a.m.ArchiveFailed.Set(float64(arc.Failed))
		if !arc.LastArchived.IsZero() {
			a.m.ArchiveLastSuccess.Set(float64(arc.LastArchived.Unix()))
		}
		if !arc.LastFailed.IsZero() {
			a.m.ArchiveLastFailure.Set(float64(arc.LastFailed.Unix()))
		}
	}
	if reps, err := a.node.Replicas(ctx); err == nil {
		a.m.ReplicaLagBytes.Reset()
		for _, r := range reps {
			a.m.ReplicaLagBytes.WithLabelValues(r.Name, r.SyncState).Set(float64(r.ReplayLag))
		}
	}
}

// handover stops a primary cleanly and releases the Lease to a successor, for
// a requested switchover or a pod shutdown. A fast shutdown sends all WAL to
// the attached standbys before the postmaster exits, so the successor has
// every commit.
func (a *Agent) handover(ctx context.Context, rec lease.Record, target string, final bool) {
	res := a.peers.FetchAll(ctx, a.others())
	succ := ""
	if target != "" && target != "any" && target != a.me {
		if r, ok := res[target]; ok && r.Err == nil && r.Status.Streaming && r.Status.Eligible {
			succ = target
		} else {
			a.log.Warn("requested switchover target is not a streaming standby", golog.F("target", target))
		}
	}
	if succ == "" && (target == "any" || target == "" || final) {
		succ = election.Successor(a.topo, a.me, a.cfg.Revision, res)
	}
	if succ == "" && !final {
		a.log.Warn("switchover requested but no standby can take over; ignoring the request")
		_ = a.lease.Annotate(ctx, map[string]string{lease.Switchover: ""})
		return
	}
	a.log.Info("handing over", golog.F("successor", succ), golog.F("shutdown", final))
	sctx, cancel := context.WithTimeout(ctx, a.cfg.ShutdownTimeout)
	defer cancel()
	_ = a.node.SetSignal(RejoinMarker, true)
	// A clean stop can outlast the lease (a busy checkpoint, slow standbys),
	// and at shutdown the control loop and its watchdog have already ended.
	// Keep renewing until the server is down, and fence if renewals fail,
	// so the Lease never lapses under a server that still takes writes.
	stopHold := a.holdWhileStopping(ctx, rec)
	if err := a.node.Stop(sctx, pg.StopFast); err != nil {
		a.log.Error(err, "stop during handover failed")
	}
	stopHold()
	a.holdingRW.Store(false)
	a.node.CloseConns()
	a.primarySince = time.Time{}
	a.m.LeaseHolder.Set(0)
	cur, err := a.lease.Get(ctx)
	if err != nil || cur.Holder != a.me {
		return
	}
	ann := map[string]string{lease.Switchover: "", lease.Successor: succ, lease.SuccessorAt: a.now().UTC().Format(time.RFC3339)}
	if _, err := a.lease.Release(ctx, cur, a.me, ann); err != nil {
		a.log.Warn("cannot release the lease; it expires on its own", golog.F("error", err.Error()))
		return
	}
	a.setRole(ctx, kube.RoleNone)
	a.log.Info("lease released", golog.F("successor", succ))
}

// holdWhileStopping renews the Lease every retry period and applies the
// watchdog's fencing rule until the returned function is called.
func (a *Agent) holdWhileStopping(ctx context.Context, rec lease.Record) func() {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(a.cfg.RetryPeriod)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if nr, err := a.renew(ctx, rec, local{}); err == nil {
					rec = nr
				}
				a.checkFence()
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}

// Shutdown stops the member for a pod termination: a primary hands over, a
// standby stops cleanly.
func (a *Agent) Shutdown(ctx context.Context) {
	if a.task != nil {
		a.log.Info("waiting for the running operation before shutdown", golog.F("operation", a.task.name))
		select {
		case <-a.task.done:
		case <-ctx.Done():
		}
	}
	rec, err := a.lease.Get(ctx)
	if err == nil && rec.Holder == a.me {
		if a.holdingRW.Load() {
			a.handover(ctx, rec, "", true)
			return
		}
		_, _ = a.lease.Release(ctx, rec, a.me, nil)
	}
	_ = a.node.Stop(ctx, pg.StopFast)
	a.holdingRW.Store(false)
}
