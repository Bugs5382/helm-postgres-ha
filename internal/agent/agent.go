// Package agent is the per-member control loop. Every retry period it reads
// the cluster Lease and the local server's state and moves the member one step
// toward its role:
//
//   - PostgreSQL runs read-write only while this member holds the Lease. A
//     holder that cannot renew within the renew deadline stops PostgreSQL
//     (fencing), well before any other member can see the Lease expire.
//   - A member that does not hold the Lease runs as a standby of the holder,
//     rewinding or re-cloning itself first when its data may have diverged.
//   - When the Lease has no live holder, the most complete standby takes it,
//     but only after a quorum of members confirms none still sees a primary.
//
// Long operations (bootstrap, clone, rewind, promotion) run in the background
// so the loop keeps renewing the Lease and answering probes.
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
	"sync"
	"sync/atomic"
	"time"

	golog "github.com/Bugs5382/go-log"

	"github.com/Bugs5382/helm-postgres-ha/internal/cluster"
	"github.com/Bugs5382/helm-postgres-ha/internal/config"
	"github.com/Bugs5382/helm-postgres-ha/internal/errs"
	"github.com/Bugs5382/helm-postgres-ha/internal/kube"
	"github.com/Bugs5382/helm-postgres-ha/internal/lease"
	"github.com/Bugs5382/helm-postgres-ha/internal/metrics"
	"github.com/Bugs5382/helm-postgres-ha/internal/peer"
	"github.com/Bugs5382/helm-postgres-ha/internal/pg"
)

// Options wires an Agent.
type Options struct {
	Config  config.Config
	Log     golog.Logger
	Node    Node
	Leases  Leases
	Peers   Peers
	Kube    Kube
	Metrics *metrics.Set
	// Backups is nil when backups are off.
	Backups Backups
	// Poolers is nil when PgBouncer is off.
	Poolers Poolers
	// ReadSecret reads a mounted password file.
	ReadSecret func(path string) (string, error)
	// LoadSpec reads the role and database spec.
	LoadSpec func(path string) (pg.Spec, error)
	// FileStamp returns a value that changes when a file changes.
	FileStamp func(path string) string
	Now       func() time.Time
}

// Agent is one member's control loop.
type Agent struct {
	cfg     config.Config
	topo    cluster.Topology
	me      string
	log     golog.Logger
	node    Node
	lease   Leases
	peers   Peers
	kube    Kube
	m       *metrics.Set
	bk      Backups
	poolers Poolers
	read    func(string) (string, error)
	spec    func(string) (pg.Spec, error)
	stamp   func(string) string
	now     func() time.Time

	// Read by the watchdog and the HTTP handlers.
	lastRenew atomic.Int64 // unix nanos of the send time of the last successful renew
	holdingRW atomic.Bool  // this member holds the Lease and runs (or is making) PostgreSQL read-write
	lastTick  atomic.Int64
	ready     atomic.Bool
	status    atomic.Pointer[peer.Status]

	// Owned by the loop goroutine.
	runCtx       context.Context
	task         *task
	retryAt      time.Time
	following    string
	notStreaming time.Time
	labelled     string
	primarySince time.Time
	lastDuty     time.Time
	fileStamps   string
	passStamp    string
	fatal        error
	lastWait     string
	lastWaitAt   time.Time
	bootstrapped bool
	split        bool
	lastDisk     time.Time
	syncNames    string
	// lastRejoinNoop is true when the last rejoin found nothing to rewind;
	// being stuck again after that means WAL is missing, not diverged.
	lastRejoinNoop bool

	// Background work that must not hold up the loop.
	bg         sync.WaitGroup
	rolesBusy  atomic.Bool
	rolesMu    sync.Mutex
	rolesStamp string
}

type task struct {
	name string
	done chan struct{}
	err  error
}

// New returns an Agent.
func New(o Options) *Agent {
	a := &Agent{
		cfg: o.Config, topo: o.Config.Topology, me: o.Config.PodName, log: o.Log,
		node: o.Node, lease: o.Leases, peers: o.Peers, kube: o.Kube, m: o.Metrics, bk: o.Backups, poolers: o.Poolers,
		read: o.ReadSecret, spec: o.LoadSpec, stamp: o.FileStamp, now: o.Now,
		runCtx: context.Background(),
	}
	if a.now == nil {
		a.now = time.Now
	}
	a.status.Store(&peer.Status{Pod: a.me, Role: peer.RoleStopped})
	return a
}

// Status is the member's last published status, for the peer API.
func (a *Agent) Status() peer.Status { return *a.status.Load() }

// Ready reports readiness for the member's role.
func (a *Agent) Ready() bool { return a.ready.Load() }

// Live returns an error when the loop has stopped ticking.
func (a *Agent) Live() error {
	last := a.lastTick.Load()
	if last == 0 {
		return nil
	}
	if age := a.now().Sub(time.Unix(0, last)); age > 5*a.cfg.RetryPeriod+a.cfg.PeerTimeout {
		return fmt.Errorf("control loop last ticked %s ago", age.Round(time.Second))
	}
	return nil
}

// Fresh reports whether the published status is recent enough for peers to
// act on.
func (a *Agent) Fresh() bool { return a.Live() == nil && a.lastTick.Load() != 0 }

// CheckVersion refuses to run a data directory made by another major version.
func (a *Agent) CheckVersion(ctx context.Context) error {
	has, err := a.node.HasData()
	if err != nil || !has {
		return err
	}
	dm, err := a.node.DataMajor()
	if err != nil {
		return err
	}
	sm, err := a.node.ServerMajor(ctx)
	if err != nil {
		return err
	}
	if dm != sm {
		return errs.New(errs.MajorVersion, fmt.Errorf("data directory is PostgreSQL %d but the server is %d; follow the major upgrade runbook", dm, sm))
	}
	return nil
}

// Run ticks until ctx ends, then shuts the member down cleanly.
func (a *Agent) Run(ctx context.Context) error {
	a.runCtx = ctx
	a.prepareBackupPath()
	if err := a.CheckVersion(ctx); err != nil {
		a.fatal = err
		a.logCoded(err, "refusing to start postgres")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); a.watchdog(ctx) }()
	t := time.NewTicker(a.cfg.RetryPeriod)
	defer t.Stop()
	a.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
			a.Shutdown(sctx)
			cancel()
			wg.Wait()
			return nil
		case <-t.C:
			a.Tick(ctx)
		}
	}
}

// watchdog fences the primary when renewals stop: it never waits on the loop,
// so a stuck round cannot keep a read-write server running without the Lease.
func (a *Agent) watchdog(ctx context.Context) {
	t := time.NewTicker(a.cfg.RetryPeriod / 4)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.checkFence()
		}
	}
}

// checkFence stops PostgreSQL immediately when this member runs read-write
// and its last successful renew is older than the renew deadline.
func (a *Agent) checkFence() {
	if !a.holdingRW.Load() {
		return
	}
	since := a.now().Sub(time.Unix(0, a.lastRenew.Load()))
	if since <= a.cfg.RenewDeadline {
		return
	}
	a.holdingRW.Store(false)
	err := errs.New(errs.Fenced, fmt.Errorf("last lease renew was %s ago, over the %s deadline", since.Round(time.Millisecond), a.cfg.RenewDeadline))
	a.logCoded(err, "fencing: stopping postgres immediately")
	a.m.Fencings.Inc()
	_ = a.node.SetSignal(RejoinMarker, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if serr := a.node.Stop(ctx, pg.StopImmediate); serr != nil {
		a.log.Error(serr, "fencing stop failed")
	}
}

func (a *Agent) logCoded(err error, msg string, fields ...golog.Field) {
	code := errs.Code(err)
	if code != 0 {
		a.m.CodedErrors.WithLabelValues(strconv.Itoa(code)).Inc()
		fields = append(fields, golog.F("code", code))
	}
	a.log.Error(err, msg, fields...)
}

// wait logs why the member is waiting, at most once a minute per reason.
func (a *Agent) wait(reason string) {
	if reason == a.lastWait && a.now().Sub(a.lastWaitAt) < time.Minute {
		golog.Trace(a.log, "waiting", golog.F("reason", reason))
		return
	}
	a.lastWait, a.lastWaitAt = reason, a.now()
	a.log.Info("waiting", golog.F("reason", reason))
}

// startTask runs fn in the background. The loop keeps renewing a held Lease
// and skips everything else until fn returns.
func (a *Agent) startTask(name string, fn func(ctx context.Context) error) {
	t := &task{name: name, done: make(chan struct{})}
	a.task = t
	a.log.Info("starting operation", golog.F("operation", name))
	start := a.now()
	go func() {
		defer close(t.done)
		t.err = fn(a.runCtx)
		if t.err != nil {
			a.logCoded(t.err, "operation failed", golog.F("operation", name), golog.F("elapsed_ms", a.now().Sub(start).Milliseconds()))
		} else {
			a.log.Info("operation finished", golog.F("operation", name), golog.F("elapsed_ms", a.now().Sub(start).Milliseconds()))
		}
	}()
}

// taskRunning reports whether a background operation is in progress, and
// collects a finished one.
func (a *Agent) taskRunning() bool {
	if a.task == nil {
		return false
	}
	select {
	case <-a.task.done:
		if a.task.err != nil {
			a.retryAt = a.now().Add(5 * a.cfg.RetryPeriod)
		}
		a.task = nil
		return false
	default:
		return true
	}
}

// WaitBackground waits for background role work; tests use it.
func (a *Agent) WaitBackground() { a.bg.Wait() }

// WaitTask blocks until the current background operation ends; tests use it.
func (a *Agent) WaitTask() error {
	if a.task == nil {
		return nil
	}
	<-a.task.done
	return a.task.err
}

// local is one observation of the local server.
type local struct {
	hasData   bool
	running   bool
	up        bool
	st        pg.Status
	systemID  string
	standby   bool // standby.signal present
	recovery  bool // recovery.signal present (restore in progress)
	rejoin    bool // rejoin marker present
	lastWasRW bool
}

func (a *Agent) observe(ctx context.Context) (local, error) {
	var l local
	var err error
	if l.hasData, err = a.node.HasData(); err != nil {
		return l, err
	}
	l.running = a.node.Running()
	if !l.hasData {
		return l, nil
	}
	l.standby = a.node.HasSignal(pg.StandbySignal)
	l.recovery = a.node.HasSignal(pg.RecoverySignal)
	l.rejoin = a.node.HasSignal(RejoinMarker)
	l.lastWasRW = !l.standby && !l.recovery
	if l.running {
		qctx, cancel := context.WithTimeout(ctx, a.cfg.PeerTimeout)
		st, serr := a.node.Status(qctx)
		cancel()
		if serr == nil {
			l.up, l.st, l.systemID = true, st, st.SystemID
		} else {
			golog.Trace(a.log, "local status unavailable", golog.F("error", serr.Error()))
		}
	}
	if l.systemID == "" && !l.running {
		if c, cerr := a.node.Control(ctx); cerr == nil {
			l.systemID = c.SystemID
			l.st.Timeline = c.Timeline
		}
	}
	return l, nil
}

// Tick runs one reconcile round.
func (a *Agent) Tick(ctx context.Context) {
	start := a.now()
	defer func() {
		a.lastTick.Store(a.now().UnixNano())
		a.m.LastTick.Set(float64(a.now().Unix()))
		a.m.TickSeconds.Observe(a.now().Sub(start).Seconds())
	}()

	rec, err := a.lease.Get(ctx)
	if err != nil {
		a.logCoded(errs.New(errs.LeaseUnreadable, err), "cannot read the cluster lease")
		a.publish(local{running: a.node.Running()}, lease.Record{})
		return
	}
	if a.taskRunning() {
		if rec.Holder == a.me {
			_, _ = a.renew(ctx, rec, local{})
		}
		a.publish(local{running: a.node.Running(), hasData: true}, rec)
		return
	}
	l, err := a.observe(ctx)
	if err != nil {
		a.log.Error(err, "cannot inspect the data directory")
		return
	}
	a.refreshPassFile()
	a.reloadOnChange(l)

	switch {
	case a.fatal != nil:
		a.stopIfRunning(ctx, l, "fatal error")
	case !l.hasData:
		a.noData(ctx, rec, l)
	case a.systemIDMismatch(rec, l) != nil:
		err := a.systemIDMismatch(rec, l)
		a.logCoded(err, "refusing to run this data directory")
		a.stopIfRunning(ctx, l, "system identifier mismatch")
	case a.now().Before(a.retryAt) && rec.Holder != a.me:
		// Back off after a failed operation.
	case rec.Holder == a.me:
		a.lead(ctx, rec, l)
	case rec.Holder != "" && !a.lease.Expired(rec):
		a.follow(ctx, rec, l)
	default:
		a.elect(ctx, rec, l)
	}
	if l2, err := a.observe(ctx); err == nil {
		l = l2
	}
	a.publish(l, rec)
	if a.bk != nil {
		a.m.BackupLastSuccess.Set(float64(a.bk.LastSuccess().Unix()))
		a.bk.Tick(ctx, BackupRole{
			Primary:          a.holdingRW.Load() && l.up && !l.st.InRecovery,
			StreamingStandby: l.up && l.st.InRecovery && l.st.Streaming(),
		})
	}
}

func (a *Agent) systemIDMismatch(rec lease.Record, l local) error {
	want := rec.Ann(lease.SystemID)
	if want == "" || l.systemID == "" || want == l.systemID {
		return nil
	}
	return errs.New(errs.SystemIDMatch, fmt.Errorf("data directory has system identifier %s but the cluster is %s", l.systemID, want))
}

func (a *Agent) stopIfRunning(ctx context.Context, l local, why string) {
	a.holdingRW.Store(false)
	if !l.running {
		return
	}
	a.log.Warn("stopping postgres", golog.F("reason", why))
	sctx, cancel := context.WithTimeout(ctx, a.cfg.ShutdownTimeout)
	defer cancel()
	_ = a.node.Stop(sctx, pg.StopFast)
}

// fence stops a server that runs read-write without the Lease.
func (a *Agent) fence(ctx context.Context, why string) {
	a.holdingRW.Store(false)
	err := errs.New(errs.Fenced, errors.New(why))
	a.logCoded(err, "fencing: stopping postgres immediately")
	a.m.Fencings.Inc()
	_ = a.node.SetSignal(RejoinMarker, true)
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = a.node.Stop(sctx, pg.StopImmediate)
	a.node.CloseConns()
	a.setRole(ctx, kube.RoleNone)
}

// setRole labels the pod, once per change.
func (a *Agent) setRole(ctx context.Context, role string) {
	if a.labelled == role {
		return
	}
	if err := a.kube.SetRole(ctx, a.me, role); err != nil {
		a.log.Warn("cannot label pod", golog.F("role", role), golog.F("error", err.Error()))
		return
	}
	a.labelled = role
	a.log.Debug("pod labelled", golog.F("role", role))
}
