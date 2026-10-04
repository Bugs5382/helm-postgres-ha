package commands

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
	"errors"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Bugs5382/helm-postgres-ha/internal/lease"
	"github.com/Bugs5382/helm-postgres-ha/internal/proc"
	"github.com/Bugs5382/helm-postgres-ha/internal/verify"
)

// verifyCmd restores the latest backup into a throwaway server and runs a
// check query: the chart's scheduled restore drill.
func verifyCmd() *cobra.Command {
	var (
		dir, socket, walg, backup, database, query, leaseName, ns string
		timeout                                                   time.Duration
	)
	cmd := &cobra.Command{
		Use:   "verify-restore",
		Short: "Restore the latest base backup into a throwaway server and run a check query",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if leaseName == "" {
				leaseName = os.Getenv("PGHA_BACKUP_LEASE")
			}
			if leaseName == "" {
				return errors.New("--lease is required")
			}
			cs, err := clientset()
			if err != nil {
				return err
			}
			log := logger()
			runner := proc.New()
			v := verify.Verifier{
				Steps:    &verify.LocalSteps{Runner: runner, Log: log, WalG: walg, SocketDir: socket, Env: os.Environ()},
				Recorder: lease.New(cs, namespace(ns), leaseName, time.Minute),
				Log:      log,
				Dir:      dir,
				WalG:     walg,
				Backup:   backup,
				Database: database,
				Query:    query,
				Timeout:  timeout,
				Poll:     2 * time.Second,
				Now:      time.Now,
			}
			return v.Run(cmd.Context())
		},
	}
	f := cmd.Flags()
	f.StringVar(&dir, "dir", "/verify/pgdata", "scratch data directory")
	f.StringVar(&socket, "socket-dir", "/verify/socket", "socket directory of the throwaway server")
	f.StringVar(&walg, "walg", "/pgha/bin/wal-g", "path of the wal-g binary")
	f.StringVar(&backup, "backup", "LATEST", "backup name, or LATEST")
	f.StringVar(&database, "database", "postgres", "database the check query runs in")
	f.StringVar(&query, "query", "SELECT count(*) FROM pg_class", "check query")
	f.StringVar(&leaseName, "lease", "", "backup lease to record the result on (default: PGHA_BACKUP_LEASE)")
	f.StringVar(&ns, "namespace", "", "namespace (default: the pod's)")
	f.DurationVar(&timeout, "timeout", time.Hour, "longest the WAL replay may take")
	return cmd
}
