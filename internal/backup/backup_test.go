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
	"strings"
	"sync"
	"testing"
	"time"

	golog "github.com/Bugs5382/go-log"
	coordv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Bugs5382/helm-postgres-ha/internal/agent"
	"github.com/Bugs5382/helm-postgres-ha/internal/config"
	"github.com/Bugs5382/helm-postgres-ha/internal/lease"
	"github.com/Bugs5382/helm-postgres-ha/internal/metrics"
)

type fakeWalG struct {
	mu    sync.Mutex
	calls []string
	err   error
	done  chan struct{}
}

func (f *fakeWalG) Run(_ context.Context, _ []string, _ string, args ...string) error {
	f.mu.Lock()
	f.calls = append(f.calls, strings.Join(args, " "))
	n := len(f.calls)
	f.mu.Unlock()
	if n%2 == 0 || f.err != nil {
		defer func() { f.done <- struct{}{} }()
	}
	return f.err
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func setup(t *testing.T, me, source string) (*Scheduler, *fakeWalG, *clock, *lease.Store) {
	t.Helper()
	c := &clock{t: time.Date(2026, 1, 1, 2, 59, 0, 0, time.UTC)}
	cs := fake.NewClientset(&coordv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "pg-backup", Namespace: "db"}})
	st := lease.New(cs, "db", "pg-backup", time.Hour)
	st.SetClock(c.now)
	w := &fakeWalG{done: make(chan struct{}, 4)}
	s, err := New(config.Backup{Enabled: true, Schedule: "0 3 * * *", Source: source, RetainFull: 7, RetainDays: 14, Timeout: time.Hour},
		me, "/data", "/bin/wal-g", nil, st, w, golog.Nop(), metrics.New(), c.now)
	if err != nil {
		t.Fatal(err)
	}
	return s, w, c, st
}

var standbyRole = agent.BackupRole{StreamingStandby: true}
var primaryRole = agent.BackupRole{Primary: true}

func waitDone(t *testing.T, w *fakeWalG) {
	t.Helper()
	select {
	case <-w.done:
	case <-time.After(5 * time.Second):
		t.Fatal("backup did not finish")
	}
	// Let the goroutine release the lease.
	time.Sleep(50 * time.Millisecond)
}

func TestScheduledBackupRunsOnceOnAStandby(t *testing.T) {
	s, w, c, st := setup(t, "pg-1", config.BackupFromStandby)
	ctx := context.Background()
	s.Tick(ctx, standbyRole)
	if len(w.calls) != 0 {
		t.Fatal("ran before the schedule")
	}
	c.add(2 * time.Minute)
	s.Tick(ctx, standbyRole)
	waitDone(t, w)
	if len(w.calls) != 2 || w.calls[0] != "backup-push /data" || !strings.HasPrefix(w.calls[1], "delete retain FULL 7 --after 2025-12-18T03:01:00Z --confirm") {
		t.Fatalf("calls = %q", w.calls)
	}
	r, _ := st.Get(ctx)
	if r.Holder != "" || r.Ann(LastSuccess) == "" || r.Ann(Slot) != "2026-01-01T03:00:00Z" {
		t.Fatalf("lease after backup: %+v", r)
	}
	s.Tick(ctx, standbyRole)
	if s.LastSuccess().IsZero() {
		t.Fatal("last success not read back")
	}
	// The same slot is never taken twice.
	s.Tick(ctx, standbyRole)
	time.Sleep(50 * time.Millisecond)
	if len(w.calls) != 2 {
		t.Fatalf("slot taken twice: %q", w.calls)
	}
}

func TestPrimaryWaitsForAStandbyFirst(t *testing.T) {
	s, w, c, _ := setup(t, "pg-0", config.BackupFromStandby)
	ctx := context.Background()
	c.add(2 * time.Minute)
	s.Tick(ctx, primaryRole)
	time.Sleep(50 * time.Millisecond)
	if len(w.calls) != 0 {
		t.Fatal("primary took the slot inside the standby grace period")
	}
	c.add(standbyGrace)
	s.Tick(ctx, primaryRole)
	waitDone(t, w)
	if len(w.calls) == 0 {
		t.Fatal("primary did not step in after the grace period")
	}
}

func TestRequestNowRunsOnPrimary(t *testing.T) {
	s, w, _, _ := setup(t, "pg-0", config.BackupFromStandby)
	s.RequestNow()
	s.Tick(context.Background(), primaryRole)
	waitDone(t, w)
	if len(w.calls) == 0 || w.calls[0] != "backup-push /data" {
		t.Fatalf("calls = %q", w.calls)
	}
}

func TestFailureIsRecorded(t *testing.T) {
	s, w, c, st := setup(t, "pg-1", config.BackupFromStandby)
	w.err = errors.New("s3 unreachable")
	c.add(2 * time.Minute)
	s.Tick(context.Background(), standbyRole)
	waitDone(t, w)
	r, _ := st.Get(context.Background())
	if r.Ann(LastFailure) == "" || r.Ann(LastSuccess) != "" || r.Holder != "" {
		t.Fatalf("lease after failure: %+v", r)
	}
}

func TestNonMembersNeverBackUp(t *testing.T) {
	s, w, c, _ := setup(t, "pg-1", config.BackupFromStandby)
	c.add(time.Hour)
	s.Tick(context.Background(), agent.BackupRole{})
	time.Sleep(50 * time.Millisecond)
	if len(w.calls) != 0 {
		t.Fatal("a member that is neither primary nor streaming backed up")
	}
}

func TestBadSchedule(t *testing.T) {
	if _, err := New(config.Backup{Schedule: "every day"}, "pg-0", "/d", "w", nil, nil, nil, golog.Nop(), metrics.New(), nil); err == nil {
		t.Fatal("bad schedule accepted")
	}
}

func TestRestoreVerificationIsExported(t *testing.T) {
	s, _, _, st := setup(t, "pg-1", config.BackupFromStandby)
	ctx := context.Background()
	if err := st.Annotate(ctx, map[string]string{"postgres-ha/last-verify": "2026-01-01T05:00:00Z", "postgres-ha/last-verify-result": "ok"}); err != nil {
		t.Fatal(err)
	}
	s.Tick(ctx, standbyRole)
	g, _ := s.m.Gather()
	if g["pgha_backup_last_verify_timestamp_seconds"] != 1767243600 || g["pgha_backup_last_verify_ok"] != 1 {
		t.Fatalf("metrics = %v %v", g["pgha_backup_last_verify_timestamp_seconds"], g["pgha_backup_last_verify_ok"])
	}
	if err := st.Annotate(ctx, map[string]string{"postgres-ha/last-verify-result": "failed"}); err != nil {
		t.Fatal(err)
	}
	s.Tick(ctx, standbyRole)
	g, _ = s.m.Gather()
	if g["pgha_backup_last_verify_ok"] != 0 {
		t.Fatal("failed verification exported as ok")
	}
}
