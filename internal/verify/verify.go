// Package verify proves a cluster's backups restore. It fetches the latest
// WAL-G base backup into a scratch directory, replays the archived WAL in a
// throwaway server that listens only on a local socket and never archives,
// runs a check query, and records the outcome on the backup Lease, where the
// members export it as a metric.
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
	"fmt"
	"time"

	golog "github.com/Bugs5382/go-log"

	"github.com/Bugs5382/helm-postgres-ha/internal/lease"
)

// Annotation keys on the backup Lease.
const (
	LastVerify       = lease.Prefix + "last-verify"
	LastVerifyResult = lease.Prefix + "last-verify-result"
)

// ErrServerExited means the throwaway server stopped during the replay; the
// check fails at once rather than waiting out its timeout.
var ErrServerExited = errors.New("the throwaway server exited during recovery")

// Steps are the operations a verification runs.
type Steps interface {
	Fetch(ctx context.Context, dir, backup string) error
	Start(dir string, settings map[string]string) error
	InRecovery(ctx context.Context) (bool, error)
	Query(ctx context.Context, database, query string) (string, error)
	Stop(ctx context.Context) error
}

// Recorder writes the outcome.
type Recorder interface {
	Annotate(ctx context.Context, ann map[string]string) error
}

// Verifier runs one verification.
type Verifier struct {
	Steps    Steps
	Recorder Recorder
	Log      golog.Logger
	// Dir is the scratch data directory.
	Dir string
	// WalG is the wal-g binary used as the restore command.
	WalG     string
	Backup   string
	Database string
	Query    string
	// Timeout bounds the WAL replay.
	Timeout time.Duration
	Poll    time.Duration
	Now     func() time.Time
}

// Settings are the throwaway server's overrides: no TCP, no archiving, WAL
// from the cluster's archive, and promotion at the end of it.
func (v Verifier) Settings() map[string]string {
	walg := v.WalG
	if walg == "" {
		walg = "wal-g"
	}
	return map[string]string{
		"listen_addresses":         "",
		"archive_mode":             "off",
		"restore_command":          walg + ` wal-fetch "%f" "%p"`,
		"recovery_target_timeline": "latest",
		// No queries run during the replay, and a hot standby would refuse
		// WAL from a primary with higher max_connections than the backup's
		// default configuration.
		"hot_standby":               "off",
		"synchronous_standby_names": "",
		"primary_conninfo":          "",
	}
}

// Run verifies once. It records the result whether or not it succeeds.
func (v Verifier) Run(ctx context.Context) error {
	start := v.Now()
	err := v.run(ctx)
	result := "ok"
	if err != nil {
		result = "failed"
		v.Log.Error(err, "restore verification failed", golog.F("elapsed_ms", v.Now().Sub(start).Milliseconds()))
	} else {
		v.Log.Info("restore verified", golog.F("elapsed_ms", v.Now().Sub(start).Milliseconds()))
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if aerr := v.Recorder.Annotate(rctx, map[string]string{LastVerify: v.Now().UTC().Format(time.RFC3339), LastVerifyResult: result}); aerr != nil {
		err = errors.Join(err, fmt.Errorf("record the result: %w", aerr))
	}
	return err
}

func (v Verifier) run(ctx context.Context) (err error) {
	v.Log.Info("fetching the base backup", golog.F("backup", v.Backup))
	if err := v.Steps.Fetch(ctx, v.Dir, v.Backup); err != nil {
		return fmt.Errorf("fetch backup %s: %w", v.Backup, err)
	}
	if err := v.Steps.Start(v.Dir, v.Settings()); err != nil {
		return fmt.Errorf("start the throwaway server: %w", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if serr := v.Steps.Stop(sctx); serr != nil {
			err = errors.Join(err, serr)
		}
	}()
	rctx, cancel := context.WithTimeout(ctx, v.Timeout)
	defer cancel()
	for {
		in, ierr := v.Steps.InRecovery(rctx)
		if errors.Is(ierr, ErrServerExited) {
			return ierr
		}
		if ierr == nil && !in {
			break
		}
		select {
		case <-rctx.Done():
			return fmt.Errorf("WAL replay did not finish within %s", v.Timeout)
		case <-time.After(v.Poll):
		}
	}
	out, err := v.Steps.Query(ctx, v.Database, v.Query)
	if err != nil {
		return fmt.Errorf("check query: %w", err)
	}
	v.Log.Info("check query answered", golog.F("database", v.Database), golog.F("result", out))
	return nil
}
