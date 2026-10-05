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
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	golog "github.com/Bugs5382/go-log"

	"github.com/Bugs5382/helm-postgres-ha/internal/proc"
)

// StopMode is a PostgreSQL shutdown mode.
type StopMode int

// Shutdown modes, mapped to the postmaster's signals.
const (
	// StopFast rolls back open transactions and shuts down cleanly (SIGINT).
	StopFast StopMode = iota
	// StopImmediate quits without a checkpoint; the next start runs crash
	// recovery (SIGQUIT). Used for fencing.
	StopImmediate
)

func (m StopMode) String() string {
	if m == StopImmediate {
		return "immediate"
	}
	return "fast"
}

func (m StopMode) signal() syscall.Signal {
	if m == StopImmediate {
		return syscall.SIGQUIT
	}
	return syscall.SIGINT
}

// ErrNotRunning is returned when an operation needs a running postmaster.
var ErrNotRunning = errors.New("postgres is not running")

// Server supervises the postmaster as a child of the agent.
type Server struct {
	runner     *proc.Runner
	log        golog.Logger
	binary     string
	dataDir    string
	configFile string
	hbaFile    string
	env        []string
	fatal      *fatalWatch

	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

// NewServer returns a supervisor for the postmaster binary at path.
func NewServer(runner *proc.Runner, log golog.Logger, binary, dataDir, configFile, hbaFile string, env []string) *Server {
	return &Server{runner: runner, log: log, binary: binary, dataDir: dataDir, configFile: configFile, hbaFile: hbaFile, env: env, fatal: newFatalWatch(os.Stderr)}
}

// Start starts the postmaster. It returns once the process runs; readiness is
// the caller's to wait for.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil {
		return nil
	}
	cmd := exec.Command(s.binary, "-D", s.dataDir, "-c", "config_file="+s.configFile, "-c", "hba_file="+s.hbaFile) // #nosec G204 -- fixed binary and agent-owned paths
	cmd.Env = s.env
	cmd.Stdout = os.Stdout
	s.fatal.Reset()
	cmd.Stderr = s.fatal
	// Its own process group, so a forced kill reaches the backends too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := s.runner.Start(cmd); err != nil {
		return fmt.Errorf("start postgres: %w", err)
	}
	done := make(chan struct{})
	s.cmd, s.done, s.err = cmd, done, nil
	s.log.Info("postgres started", golog.F("pid", cmd.Process.Pid), golog.F("data_dir", s.dataDir))
	go func() {
		err := s.runner.Wait(cmd)
		s.mu.Lock()
		s.cmd, s.err = nil, err
		s.mu.Unlock()
		if err != nil {
			s.log.Warn("postgres exited", golog.F("pid", cmd.Process.Pid), golog.F("error", err.Error()))
		} else {
			s.log.Info("postgres exited", golog.F("pid", cmd.Process.Pid))
		}
		close(done)
	}()
	return nil
}

// LastFatal returns the FATAL or PANIC message the current or last
// postmaster logged, or "" when it logged none.
func (s *Server) LastFatal() string { return s.fatal.Last() }

// Running reports whether the postmaster process is alive.
func (s *Server) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmd != nil
}

// Done returns a channel closed when the current postmaster exits, or nil
// when none runs.
func (s *Server) Done() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil {
		return nil
	}
	return s.done
}

func (s *Server) process() (*exec.Cmd, chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmd, s.done
}

// Signal sends a signal to the postmaster.
func (s *Server) Signal(sig syscall.Signal) error {
	cmd, _ := s.process()
	if cmd == nil {
		return ErrNotRunning
	}
	return cmd.Process.Signal(sig)
}

// Reload asks the postmaster to re-read its configuration files and
// certificates.
func (s *Server) Reload() error {
	return s.Signal(syscall.SIGHUP)
}

// Stop shuts the postmaster down in the given mode and waits until it exits.
// When ctx ends first it escalates to an immediate shutdown and then kills the
// whole process group.
func (s *Server) Stop(ctx context.Context, mode StopMode) error {
	cmd, done := s.process()
	if cmd == nil {
		return nil
	}
	start := time.Now()
	s.log.Info("stopping postgres", golog.F("mode", mode.String()), golog.F("pid", cmd.Process.Pid))
	if err := cmd.Process.Signal(mode.signal()); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("signal postgres: %w", err)
	}
	select {
	case <-done:
		s.log.Info("postgres stopped", golog.F("mode", mode.String()), golog.F("elapsed_ms", time.Since(start).Milliseconds()))
		return nil
	case <-ctx.Done():
	}
	if mode != StopImmediate {
		s.log.Warn("postgres did not stop in time, escalating to immediate", golog.F("mode", mode.String()))
		_ = cmd.Process.Signal(StopImmediate.signal())
		select {
		case <-done:
			return nil
		case <-time.After(5 * time.Second):
		}
	}
	s.log.Error(errors.New("postgres did not stop"), "killing the postgres process group", golog.F("pid", cmd.Process.Pid))
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	<-done
	return nil
}
