package agent

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
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Bugs5382/helm-postgres-ha/internal/cluster"
	"github.com/Bugs5382/helm-postgres-ha/internal/config"
	"github.com/Bugs5382/helm-postgres-ha/internal/pg"
)

// conninfo builds a libpq connection string to a member as user. Every
// connection between members is TLS with full verification against the
// cluster CA, and fails fast when the member is gone.
func (a *Agent) conninfo(pod, user, dbname string) string {
	parts := []string{
		"host=" + a.topo.Host(pod),
		"port=5432",
		"user=" + user,
		"passfile=" + a.cfg.PassFile(),
		"sslmode=verify-full",
		"sslrootcert=" + filepath.Join(a.cfg.TLSDir, "ca.crt"),
		"application_name=" + a.me,
		"connect_timeout=5",
		"keepalives=1",
		"keepalives_idle=10",
		"keepalives_interval=5",
		"keepalives_count=3",
	}
	if dbname != "" {
		parts = append(parts, "dbname="+dbname)
	}
	return strings.Join(parts, " ")
}

// restoreCommand fetches archived WAL from prefix, or from the cluster's own
// storage when prefix is empty.
func (a *Agent) restoreCommand(prefix string) string {
	cmd := a.cfg.WalG() + ` wal-fetch "%f" "%p"`
	if prefix != "" {
		cmd = "WALG_S3_PREFIX=" + shellQuote(prefix) + " " + cmd
	}
	return cmd
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// standbySettings follow holder, or nobody when holder is empty (a member
// waiting for an election replays its own WAL and reports its position).
func (a *Agent) standbySettings(holder string) map[string]string {
	s := map[string]string{"recovery_target_timeline": "latest", "synchronous_standby_names": ""}
	if holder != "" {
		s["primary_conninfo"] = a.conninfo(holder, pg.RoleReplication, "")
		s["primary_slot_name"] = cluster.SlotName(a.me)
	}
	if a.cfg.Backup.Enabled {
		s["restore_command"] = a.restoreCommand("")
	}
	return s
}

func (a *Agent) primarySettings() map[string]string {
	// recovery_target_timeline only takes effect at start-up; keeping it the
	// same in every role avoids a "cannot be changed" warning on each reload.
	s := map[string]string{"synchronous_standby_names": a.syncNames, "recovery_target_timeline": "latest"}
	if a.cfg.Backup.Enabled {
		s["restore_command"] = a.restoreCommand("")
	}
	return s
}

// restoreSettings drive archive recovery from a backup up to the target, then
// promote.
func (a *Agent) restoreSettings() map[string]string {
	r := a.cfg.Bootstrap.Restore
	s := map[string]string{
		"restore_command":          a.restoreCommand(r.Prefix),
		"recovery_target_timeline": "latest",
	}
	target := false
	switch {
	case r.TargetTime != "":
		s["recovery_target_time"], target = r.TargetTime, true
	case r.TargetLSN != "":
		s["recovery_target_lsn"], target = r.TargetLSN, true
	case r.TargetName != "":
		s["recovery_target_name"], target = r.TargetName, true
	}
	if target {
		s["recovery_target_action"] = "promote"
		s["recovery_target_inclusive"] = fmt.Sprint(!r.TargetExclusive)
	}
	return s
}

// restoreTarget describes the configured recovery target for messages.
func (a *Agent) restoreTarget() string {
	r := a.cfg.Bootstrap.Restore
	switch {
	case r.TargetTime != "":
		return "time " + r.TargetTime
	case r.TargetLSN != "":
		return "LSN " + r.TargetLSN
	case r.TargetName != "":
		return "name " + r.TargetName
	}
	return "(none)"
}

// desiredSync renders synchronous_standby_names from the standbys that are
// streaming now. Listing only attached standbys keeps a fresh primary (or one
// whose standbys are all down) from blocking every commit; strict mode lists
// every other member and accepts that block. Standbys are named by pod, the
// application_name each one connects with.
func (a *Agent) desiredSync(streaming []string) string {
	names := streaming
	switch a.cfg.SyncMode {
	case config.SyncOff:
		return ""
	case config.SyncStrict:
		names = a.others()
	}
	if len(names) == 0 {
		return ""
	}
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	quoted := make([]string, len(sorted))
	for i, n := range sorted {
		quoted[i] = `"` + n + `"`
	}
	return fmt.Sprintf("ANY %d (%s)", min(a.cfg.SyncCount, len(sorted)), strings.Join(quoted, ", "))
}

// passEntries are the credentials the replication and rewind connections use.
func (a *Agent) passEntries() ([]pg.PassEntry, error) {
	rep, err := a.read(a.cfg.SecretFile("replication"))
	if err != nil {
		return nil, err
	}
	rw, err := a.read(a.cfg.SecretFile("rewind"))
	if err != nil {
		return nil, err
	}
	return []pg.PassEntry{{User: pg.RoleReplication, Password: rep}, {User: pg.RoleRewind, Password: rw}}, nil
}

func (a *Agent) credentials() (pg.Credentials, error) {
	var c pg.Credentials
	var err error
	if c.Superuser, err = a.read(a.cfg.SecretFile("superuser")); err != nil {
		return c, err
	}
	if c.Replication, err = a.read(a.cfg.SecretFile("replication")); err != nil {
		return c, err
	}
	if c.Rewind, err = a.read(a.cfg.SecretFile("rewind")); err != nil {
		return c, err
	}
	if a.cfg.PgBouncerService != "" {
		if c.PgBouncer, err = a.read(a.cfg.SecretFile("pgbouncer")); err != nil {
			return c, err
		}
	}
	return c, nil
}

// bootstrapMode is the configured way an empty cluster gets its data.
func (a *Agent) restoring() bool { return a.cfg.Bootstrap.Mode == config.BootstrapRestore }
