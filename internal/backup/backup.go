// Package backup schedules WAL-G base backups from inside the cluster, with
// no CronJob and no exec rights: every member evaluates the schedule, and the
// one that claims the backup Lease runs wal-g against its own data directory.
// The Lease's annotations record the last slot taken and the last success, so
// a backup is never taken twice for one slot and its age is visible from any
// member.
package backup

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
	"time"

	golog "github.com/Bugs5382/go-log"
	"github.com/robfig/cron/v3"

	"github.com/Bugs5382/helm-postgres-ha/internal/agent"
	"github.com/Bugs5382/helm-postgres-ha/internal/config"
	"github.com/Bugs5382/helm-postgres-ha/internal/errs"
	"github.com/Bugs5382/helm-postgres-ha/internal/lease"
	"github.com/Bugs5382/helm-postgres-ha/internal/metrics"
)

// Annotation keys on the backup Lease.
const (
	Slot        = lease.Prefix + "slot"
	LastSuccess = lease.Prefix + "last-success"
	LastBackup  = lease.Prefix + "last-backup"
	LastFailure = lease.Prefix + "last-failure"
	// Request asks for a backup now: any RFC 3339 time later than the last
	// slot taken.
	Request = lease.Prefix + "request"
)

// standbyGrace is how long a primary waits for a standby to take a backup
// before it takes the slot itself.
const standbyGrace = 5 * time.Minute

// Leases is the backup Lease.
type Leases interface {
	Get(ctx context.Context) (lease.Record, error)
	Expired(r lease.Record) bool
	Acquire(ctx context.Context, r lease.Record, me string, ann map[string]string) (lease.Record, error)
	Renew(ctx context.Context, r lease.Record, me string, ann map[string]string) (lease.Record, error)
	Release(ctx context.Context, r lease.Record, me string, ann map[string]string) (lease.Record, error)
}

// WalG runs wal-g.
type WalG interface {
	Run(ctx context.Context, env []string, name string, args ...string) error
}

// Scheduler is one member's backup scheduler.
type Scheduler struct {
	cfg      config.Backup
	me       string
	dataDir  string
	walg     string
	env      []string
	leases   Leases
	run      WalG
	log      golog.Logger
	m        *metrics.Set
	now      func() time.Time
	schedule cron.Schedule
	started  time.Time

	mu          sync.Mutex
	running     bool
	requested   bool
	lastSuccess time.Time
}

// New parses the schedule and returns a Scheduler.
func New(cfg config.Backup, me, dataDir, walg string, env []string, leases Leases, run WalG, log golog.Logger, m *metrics.Set, now func() time.Time) (*Scheduler, error) {
	sched, err := cron.ParseStandard(cfg.Schedule)
	if err != nil {
		return nil, errs.New(errs.Config, fmt.Errorf("backup schedule %q: %w", cfg.Schedule, err))
	}
	if now == nil {
		now = time.Now
	}
	return &Scheduler{cfg: cfg, me: me, dataDir: dataDir, walg: walg, env: env, leases: leases, run: run, log: log, m: m, now: now, schedule: sched, started: now()}, nil
}

// RequestNow asks for a backup on the next round this member can take it,
// used right after a bootstrap so point-in-time recovery has a base.
func (s *Scheduler) RequestNow() {
	s.mu.Lock()
	s.requested = true
	s.mu.Unlock()
}

// LastSuccess is the last successful backup as recorded on the Lease.
func (s *Scheduler) LastSuccess() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSuccess
}

func parseTime(v string) time.Time {
	t, _ := time.Parse(time.RFC3339, v)
	return t
}

// due returns the slot to take, or the zero time when nothing is due.
func (s *Scheduler) due(rec lease.Record) time.Time {
	last := parseTime(rec.Ann(Slot))
	base := last
	if base.IsZero() || base.Before(s.started) {
		base = s.started
	}
	if next := s.schedule.Next(base.UTC()); !next.After(s.now().UTC()) {
		return next
	}
	if req := parseTime(rec.Ann(Request)); !req.IsZero() && req.After(last) {
		return req
	}
	return time.Time{}
}

// Tick checks whether a backup is due and this member should take it.
func (s *Scheduler) Tick(ctx context.Context, role agent.BackupRole) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	requested := s.requested
	s.mu.Unlock()

	rec, err := s.leases.Get(ctx)
	if err != nil {
		golog.Trace(s.log, "cannot read the backup lease", golog.F("error", err.Error()))
		return
	}
	s.mu.Lock()
	s.lastSuccess = parseTime(rec.Ann(LastSuccess))
	s.mu.Unlock()
	if !role.Primary && !role.StreamingStandby {
		return
	}
	slot := s.due(rec)
	if requested && role.Primary {
		slot = s.now().UTC().Truncate(time.Second)
	}
	if slot.IsZero() {
		return
	}
	if !s.mayTake(role, slot, requested) {
		return
	}
	if rec.Holder != "" && !s.leases.Expired(rec) {
		return
	}
	claimed, err := s.leases.Acquire(ctx, rec, s.me, map[string]string{Slot: slot.Format(time.RFC3339), Request: ""})
	if err != nil {
		golog.Trace(s.log, "backup slot taken by another member", golog.F("error", err.Error()))
		return
	}
	s.mu.Lock()
	s.running, s.requested = true, false
	s.mu.Unlock()
	// A backup outlives the round that started it, and is bounded by its own
	// timeout inside backup.
	go s.backup(claimed, slot) // #nosec G118 -- intentionally detached from the round's context
}

// mayTake applies the source preference: with standby as the source a
// primary only steps in after the grace period, or for its own request.
func (s *Scheduler) mayTake(role agent.BackupRole, slot time.Time, requested bool) bool {
	if s.cfg.Source == config.BackupFromPrimary {
		return role.Primary
	}
	if role.StreamingStandby {
		return true
	}
	return requested || s.now().Sub(slot) > standbyGrace
}

func (s *Scheduler) backup(rec lease.Record, slot time.Time) {
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
	defer cancel()
	var mu sync.Mutex
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				mu.Lock()
				if nr, err := s.leases.Renew(ctx, rec, s.me, nil); err == nil {
					rec = nr
				}
				mu.Unlock()
			}
		}
	}()
	start := s.now()
	s.log.Info("base backup started", golog.F("slot", slot.Format(time.RFC3339)))
	err := s.run.Run(ctx, s.env, s.walg, "backup-push", s.dataDir)
	if err == nil {
		err = s.retain(ctx)
	}
	close(stop)
	mu.Lock()
	defer mu.Unlock()
	elapsed := s.now().Sub(start)
	s.m.BackupDuration.Set(elapsed.Seconds())
	ann := map[string]string{}
	if err != nil {
		s.m.BackupFailures.Inc()
		s.m.CodedErrors.WithLabelValues(strconv.Itoa(errs.Backup)).Inc()
		s.log.Error(errs.New(errs.Backup, err), "base backup failed", golog.F("code", errs.Backup), golog.F("elapsed_ms", elapsed.Milliseconds()))
		ann[LastFailure] = s.now().UTC().Format(time.RFC3339)
	} else {
		s.log.Info("base backup finished", golog.F("elapsed_ms", elapsed.Milliseconds()))
		ann[LastSuccess] = s.now().UTC().Format(time.RFC3339)
		ann[LastBackup] = s.me
	}
	rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer rcancel()
	cur, gerr := s.leases.Get(rctx)
	if gerr != nil {
		cur = rec
	}
	if _, rerr := s.leases.Release(rctx, cur, s.me, ann); rerr != nil {
		s.log.Warn("cannot release the backup lease", golog.F("error", rerr.Error()))
	}
}

// retain deletes backups past the retention: it keeps at least RetainFull full
// backups and, when RetainDays is set, every backup newer than that window.
func (s *Scheduler) retain(ctx context.Context) error {
	args := []string{"delete", "retain", "FULL", strconv.Itoa(s.cfg.RetainFull)}
	if s.cfg.RetainDays > 0 {
		after := s.now().UTC().Add(-time.Duration(s.cfg.RetainDays) * 24 * time.Hour).Format(time.RFC3339)
		args = append(args, "--after", after)
	}
	args = append(args, "--confirm")
	if err := s.run.Run(ctx, s.env, s.walg, args...); err != nil {
		return errors.Join(errors.New("retention"), err)
	}
	return nil
}
