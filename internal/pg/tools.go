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
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	golog "github.com/Bugs5382/go-log"

	"github.com/Bugs5382/helm-postgres-ha/internal/proc"
)

// Tools runs the PostgreSQL server programs found on PATH, so the agent never
// hard-codes a version-specific install directory.
type Tools struct {
	runner *proc.Runner
	log    golog.Logger
	env    []string
}

// NewTools returns Tools that run with env.
func NewTools(runner *proc.Runner, log golog.Logger, env []string) *Tools {
	return &Tools{runner: runner, log: log, env: env}
}

// Path resolves a server program.
func (t *Tools) Path(name string) (string, error) {
	return exec.LookPath(name)
}

func (t *Tools) run(ctx context.Context, env []string, name string, args ...string) (proc.Result, error) {
	path, err := t.Path(name)
	if err != nil {
		return proc.Result{}, fmt.Errorf("find %s: %w", name, err)
	}
	t.log.Debug("running server tool", golog.F("tool", name))
	res, err := t.runner.Run(ctx, env, path, args...)
	if err != nil {
		t.log.Warn("server tool failed", golog.F("tool", name), golog.F("elapsed_ms", res.Duration.Milliseconds()), golog.F("error", err.Error()))
		return res, err
	}
	t.log.Info("server tool finished", golog.F("tool", name), golog.F("elapsed_ms", res.Duration.Milliseconds()))
	return res, nil
}

// Initdb creates a new cluster in dir with the superuser password from pwfile.
func (t *Tools) Initdb(ctx context.Context, dir, pwfile string, extra []string) error {
	args := append([]string{"-D", dir, "--username=postgres", "--pwfile=" + pwfile, "--auth-local=peer", "--auth-host=scram-sha-256"}, extra...)
	_, err := t.run(ctx, t.env, "initdb", args...)
	return err
}

// BaseBackup clones a running server into dir.
func (t *Tools) BaseBackup(ctx context.Context, dir, conninfo string) error {
	_, err := t.run(ctx, t.env, "pg_basebackup", "-d", conninfo, "-D", dir, "-X", "stream", "-c", "fast", "--no-password")
	return err
}

// Rewind synchronises a stopped data directory with a running source server.
// It reports whether a rewind was needed.
func (t *Tools) Rewind(ctx context.Context, dir, sourceConninfo, configFile string) (bool, error) {
	res, err := t.run(ctx, t.env, "pg_rewind", "-D", dir, "--source-server="+sourceConninfo, "--config-file="+configFile, "--no-sync")
	if err != nil {
		return false, err
	}
	out := res.Stdout + res.Stderr
	return !strings.Contains(out, "no rewind required"), nil
}

// Control is what the agent reads from pg_controldata.
type Control struct {
	SystemID string
	Timeline int
	State    string
}

// ControlData reads a data directory's control file. It works while the
// server is stopped.
func (t *Tools) ControlData(ctx context.Context, dir string) (Control, error) {
	env := append(append([]string{}, t.env...), "LC_ALL=C")
	res, err := t.run(ctx, env, "pg_controldata", dir)
	if err != nil {
		return Control{}, err
	}
	return parseControlData(res.Stdout)
}

func parseControlData(out string) (Control, error) {
	var c Control
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "Database system identifier":
			c.SystemID = v
		case "Latest checkpoint's TimeLineID":
			n, err := strconv.Atoi(v)
			if err != nil {
				return Control{}, fmt.Errorf("parse timeline %q: %w", v, err)
			}
			c.Timeline = n
		case "Database cluster state":
			c.State = v
		}
	}
	if c.SystemID == "" {
		return Control{}, fmt.Errorf("pg_controldata output has no system identifier")
	}
	return c, nil
}

var versionRE = regexp.MustCompile(`\(PostgreSQL\) (\d+)`)

// ServerMajor returns the major version of the installed server.
func (t *Tools) ServerMajor(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := t.run(ctx, t.env, "postgres", "--version")
	if err != nil {
		return 0, err
	}
	return parseMajor(res.Stdout)
}

func parseMajor(out string) (int, error) {
	m := versionRE.FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("unrecognised postgres --version output %q", strings.TrimSpace(out))
	}
	return strconv.Atoi(m[1])
}
