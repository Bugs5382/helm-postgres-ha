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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	golog "github.com/Bugs5382/go-log"

	"github.com/Bugs5382/helm-postgres-ha/internal/pg"
	"github.com/Bugs5382/helm-postgres-ha/internal/proc"
)

// LocalSteps runs a verification with the server tools and wal-g in the
// verification pod.
type LocalSteps struct {
	Runner    *proc.Runner
	Log       golog.Logger
	WalG      string
	SocketDir string
	Env       []string

	srv *pg.Server
	db  *pg.DB
}

// Fetch restores a base backup into dir and marks it for archive recovery.
func (l *LocalSteps) Fetch(ctx context.Context, dir, backup string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if _, err := l.Runner.Run(ctx, l.Env, l.WalG, "backup-fetch", dir, backup); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "pg_wal", "archive_status"), 0o700); err != nil {
		return err
	}
	// A backup taken from a standby, or from a member that was rejoining,
	// can carry markers that would make the throwaway server wait for a
	// primary. It only replays the archive.
	for _, f := range []string{pg.StandbySignal, "pgha.rejoin"} {
		if err := pg.SetSignal(dir, f, false); err != nil {
			return err
		}
	}
	return pg.SetSignal(dir, pg.RecoverySignal, true)
}

// Start writes the overrides to postgresql.auto.conf, which the server reads
// last, and starts it on the data directory's own configuration files.
func (l *LocalSteps) Start(dir string, settings map[string]string) error {
	s := map[string]string{"unix_socket_directories": l.SocketDir}
	for k, v := range settings {
		s[k] = v
	}
	if _, err := pg.WriteFileAtomic(filepath.Join(dir, "postgresql.auto.conf"), pg.RenderConf(s), 0o600); err != nil {
		return err
	}
	bin, err := exec.LookPath("postgres")
	if err != nil {
		return fmt.Errorf("find postgres: %w", err)
	}
	if err := os.MkdirAll(l.SocketDir, 0o700); err != nil {
		return err
	}
	l.srv = pg.NewServer(l.Runner, l.Log, bin, dir, filepath.Join(dir, "postgresql.conf"), filepath.Join(dir, "pg_hba.conf"), l.Env)
	l.db = pg.NewDB(l.SocketDir, l.Log)
	return l.srv.Start()
}

// InRecovery reports whether the server is still replaying WAL.
func (l *LocalSteps) InRecovery(ctx context.Context) (bool, error) {
	if !l.srv.Running() {
		return true, ErrServerExited
	}
	v, err := l.db.QueryStrings(ctx, "postgres", "SELECT pg_is_in_recovery()::text")
	if err != nil {
		l.db.Close()
		return true, err
	}
	return len(v) == 1 && v[0] == "true", nil
}

// Query runs the check query and returns its first column of the first row.
func (l *LocalSteps) Query(ctx context.Context, database, query string) (string, error) {
	v, err := l.db.QueryStrings(ctx, database, query)
	if err != nil {
		return "", err
	}
	if len(v) == 0 {
		return "", nil
	}
	return v[0], nil
}

// Stop stops the throwaway server.
func (l *LocalSteps) Stop(ctx context.Context) error {
	if l.db != nil {
		l.db.Close()
	}
	if l.srv == nil {
		return nil
	}
	return l.srv.Stop(ctx, pg.StopFast)
}
