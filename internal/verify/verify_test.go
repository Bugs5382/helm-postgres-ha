package verify

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
	"testing"
	"time"

	golog "github.com/Bugs5382/go-log"
)

type fakeSteps struct {
	exited    bool
	calls     []string
	fetchErr  error
	queryErr  error
	promoteIn int
	polls     int
	settings  map[string]string
}

func (f *fakeSteps) Fetch(_ context.Context, dir, backup string) error {
	f.calls = append(f.calls, "fetch "+dir+" "+backup)
	return f.fetchErr
}

func (f *fakeSteps) Start(dir string, settings map[string]string) error {
	f.calls = append(f.calls, "start "+dir)
	f.settings = settings
	return nil
}

func (f *fakeSteps) InRecovery(context.Context) (bool, error) {
	if f.exited {
		return true, ErrServerExited
	}
	f.polls++
	return f.polls <= f.promoteIn, nil
}

func (f *fakeSteps) Query(_ context.Context, db, q string) (string, error) {
	f.calls = append(f.calls, "query "+db+" "+q)
	return "42", f.queryErr
}

func (f *fakeSteps) Stop(context.Context) error {
	f.calls = append(f.calls, "stop")
	return nil
}

type fakeRecorder struct{ ann map[string]string }

func (r *fakeRecorder) Annotate(_ context.Context, ann map[string]string) error {
	r.ann = ann
	return nil
}

func run(t *testing.T, s *fakeSteps) (*fakeRecorder, error) {
	t.Helper()
	rec := &fakeRecorder{}
	v := Verifier{Steps: s, Recorder: rec, Log: golog.Nop(), Dir: "/scratch/pgdata", Backup: "LATEST",
		Database: "app", Query: "select count(*) from e2e", Timeout: time.Minute, Poll: time.Millisecond,
		Now: func() time.Time { return time.Date(2026, 1, 1, 5, 0, 0, 0, time.UTC) }}
	return rec, v.Run(context.Background())
}

func TestVerifyRestoresQueriesAndRecordsSuccess(t *testing.T) {
	s := &fakeSteps{promoteIn: 3}
	rec, err := run(t, s)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"fetch /scratch/pgdata LATEST", "start /scratch/pgdata", "query app select count(*) from e2e", "stop"}
	if strings.Join(s.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %q", s.calls)
	}
	if rec.ann[LastVerify] != "2026-01-01T05:00:00Z" || rec.ann[LastVerifyResult] != "ok" {
		t.Fatalf("annotations = %v", rec.ann)
	}
}

func TestVerifyNeverArchivesOrListens(t *testing.T) {
	s := &fakeSteps{}
	if _, err := run(t, s); err != nil {
		t.Fatal(err)
	}
	// hot_standby off: a hot standby refuses to replay WAL from a primary
	// with higher max_connections than the backup's own default config.
	for k, want := range map[string]string{"archive_mode": "off", "listen_addresses": "", "hot_standby": "off"} {
		if got, ok := s.settings[k]; !ok || got != want {
			t.Errorf("%s = %q (set %v), want %q", k, got, ok, want)
		}
	}
	if !strings.Contains(s.settings["restore_command"], "wal-fetch") {
		t.Errorf("restore_command = %q", s.settings["restore_command"])
	}
}

func TestVerifyRecordsFailures(t *testing.T) {
	for name, s := range map[string]*fakeSteps{
		"fetch": {fetchErr: errors.New("no backups")},
		"query": {queryErr: errors.New("relation does not exist")},
	} {
		rec, err := run(t, s)
		if err == nil {
			t.Errorf("%s: no error", name)
		}
		if rec.ann[LastVerifyResult] != "failed" {
			t.Errorf("%s: annotations = %v", name, rec.ann)
		}
	}
}

func TestVerifyStopsAfterAFailedQuery(t *testing.T) {
	s := &fakeSteps{queryErr: errors.New("boom")}
	_, _ = run(t, s)
	if s.calls[len(s.calls)-1] != "stop" {
		t.Fatalf("server left running: %q", s.calls)
	}
}

func TestVerifyFailsAtOnceWhenTheServerExits(t *testing.T) {
	s := &fakeSteps{exited: true}
	start := time.Now()
	rec, err := run(t, s)
	if !errors.Is(err, ErrServerExited) || time.Since(start) > time.Second {
		t.Fatalf("err = %v after %s", err, time.Since(start))
	}
	if rec.ann[LastVerifyResult] != "failed" {
		t.Fatalf("annotations = %v", rec.ann)
	}
}
