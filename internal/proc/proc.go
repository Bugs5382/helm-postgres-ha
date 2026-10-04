// Package proc runs the agent's child processes and, when the agent is PID 1
// in its container, reaps orphaned processes that are re-parented to it.
//
// A plain wait4(-1) reaper would steal the exit status of children the agent
// is waiting on itself. The reaper here only reaps zombies it finds in /proc
// whose parent is this process and that no caller registered, and it takes the
// same lock Start holds while it registers a new child.
package proc

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
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Runner starts child processes and tracks which ones have an owner.
type Runner struct {
	mu      sync.Mutex
	managed map[int]struct{}
	procDir string
}

// New returns a Runner.
func New() *Runner {
	return &Runner{managed: map[int]struct{}{}, procDir: "/proc"}
}

// Start starts cmd and registers it, so the reaper leaves it to its owner.
// The caller must call Wait.
func (r *Runner) Start(cmd *exec.Cmd) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return err
	}
	r.managed[cmd.Process.Pid] = struct{}{}
	return nil
}

// Wait waits for a command started with Start and unregisters it.
func (r *Runner) Wait(cmd *exec.Cmd) error {
	err := cmd.Wait()
	r.mu.Lock()
	delete(r.managed, cmd.Process.Pid)
	r.mu.Unlock()
	return err
}

// Result is the outcome of one command run with Run.
type Result struct {
	Stdout   string
	Stderr   string
	Duration time.Duration
}

// Run runs a command to completion, capturing its output. A context
// cancellation sends SIGTERM, then SIGKILL after five seconds.
func (r *Runner) Run(ctx context.Context, env []string, name string, args ...string) (Result, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- callers pass fixed server tools and wal-g
	cmd.Env = env
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	start := time.Now()
	if err := r.Start(cmd); err != nil {
		return Result{}, fmt.Errorf("start %s: %w", name, err)
	}
	err := r.Wait(cmd)
	res := Result{Stdout: stdout.String(), Stderr: stderr.String(), Duration: time.Since(start)}
	if err != nil {
		return res, fmt.Errorf("%s %s: %w: %s", filepath.Base(name), strings.Join(args, " "), err, lastLine(res.Stderr))
	}
	return res, nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// ReapOnce reaps every unregistered zombie child of this process and returns
// how many it reaped.
func (r *Runner) ReapOnce() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	me := os.Getpid()
	n := 0
	for _, pid := range r.zombieChildren(me) {
		if _, owned := r.managed[pid]; owned {
			continue
		}
		var ws syscall.WaitStatus
		if got, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil); err == nil && got == pid {
			n++
		}
	}
	return n
}

// ReapLoop reaps orphans until ctx ends. Run it only when this process is the
// container's init (PID 1); elsewhere the real init does it.
func (r *Runner) ReapLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.ReapOnce()
		}
	}
}

// zombieChildren lists the pids in /proc that are zombies with parent ppid.
func (r *Runner) zombieChildren(ppid int) []int {
	entries, err := os.ReadDir(r.procDir)
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		state, parent, err := readStat(filepath.Join(r.procDir, e.Name(), "stat"))
		if err != nil || parent != ppid || state != 'Z' {
			continue
		}
		out = append(out, pid)
	}
	return out
}

// readStat parses the state and parent pid out of /proc/<pid>/stat. The
// command name sits in parentheses and may itself contain spaces or
// parentheses, so parsing starts after the last ')'.
func readStat(path string) (byte, int, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- path is built from /proc entries
	if err != nil {
		return 0, 0, err
	}
	return parseStat(string(b))
}

func parseStat(s string) (byte, int, error) {
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, 0, errors.New("malformed stat: no command name")
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 || len(f[0]) != 1 {
		return 0, 0, errors.New("malformed stat: short")
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return 0, 0, fmt.Errorf("malformed stat ppid: %w", err)
	}
	return f[0][0], ppid, nil
}
