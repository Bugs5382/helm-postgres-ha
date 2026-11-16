package pg

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
	"os"
	"path/filepath"
	"testing"
	"time"

	golog "github.com/Bugs5382/go-log"

	"github.com/Bugs5382/helm-postgres-ha/internal/proc"
)

// fakePostmaster is a stand-in binary that, like the real one, refuses to run
// when postmaster.pid names a live process. Here it only records whether the
// file was there when it started.
func fakePostmaster(t *testing.T, seen string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "postgres")
	script := "#!/bin/sh\nif [ -e \"$2/postmaster.pid\" ]; then echo present > " + seen + "; else echo absent > " + seen + "; fi\nexec sleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { // #nosec G306 -- a test binary
		t.Fatal(err)
	}
	return bin
}

// After an unclean stop the data directory keeps postmaster.pid. The agent has
// no postmaster child at its first start, so the file is stale and must be gone
// before the postmaster starts: its PID can belong to any live process of the
// new container, and PostgreSQL then refuses to start.
func TestStartClearsAStalePostmasterPID(t *testing.T) {
	dir, seen := t.TempDir(), filepath.Join(t.TempDir(), "seen")
	pid := filepath.Join(dir, "postmaster.pid")
	if err := os.WriteFile(pid, []byte("16\n"+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewServer(proc.New(), golog.Nop(), fakePostmaster(t, seen), dir, filepath.Join(dir, "postgresql.conf"), filepath.Join(dir, "pg_hba.conf"), os.Environ())
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Stop(t.Context(), StopFast) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(seen); err == nil && len(b) > 0 { // #nosec G304 -- the test's own file
			if string(b) != "absent\n" {
				t.Fatalf("the postmaster started with the stale postmaster.pid still there: %q", b)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the fake postmaster never ran")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A running postmaster's own file is never touched: Start while a child runs
// does nothing.
func TestStartWithARunningPostmasterKeepsItsPID(t *testing.T) {
	dir, seen := t.TempDir(), filepath.Join(t.TempDir(), "seen")
	s := NewServer(proc.New(), golog.Nop(), fakePostmaster(t, seen), dir, filepath.Join(dir, "postgresql.conf"), filepath.Join(dir, "pg_hba.conf"), os.Environ())
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Stop(t.Context(), StopFast) }()
	pid := filepath.Join(dir, "postmaster.pid")
	if err := os.WriteFile(pid, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pid); err != nil {
		t.Fatalf("a second Start removed the running postmaster's pid file: %v", err)
	}
}
