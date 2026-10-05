//go:build integration

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
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// agentImage is the agent image the file-backend test takes wal-g from. CI
// builds it before the integration tests run.
func agentImage() string {
	if v := os.Getenv("PGHA_IT_AGENT_IMAGE"); v != "" {
		return v
	}
	return "ghcr.io/bugs5382/helm-postgres-ha/pgha:e2e"
}

// TestArchiveAndBackupToAFileBackend runs a real server that archives WAL
// with wal-g to a file prefix on a mounted directory, takes a base backup
// into it, and lists both.
func TestArchiveAndBackupToAFileBackend(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	if exec.Command("docker", "image", "inspect", agentImage()).Run() != nil {
		t.Skipf("agent image %s not built", agentImage())
	}
	id := fmt.Sprintf("pgha-it-file-%d", time.Now().UnixNano())
	base, err := os.MkdirTemp("", "pgha-it-file-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "run", "--rm", "-v", base+":/b", "--entrypoint", "sh", image, "-c", "rm -rf /b/*").Run()
		_ = os.RemoveAll(base)
	})
	tools, backup := base+"/tools", base+"/backup"
	for _, d := range []string{tools, backup} {
		if err := os.Mkdir(d, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	// The agent image is distroless; copy wal-g out of a stopped container.
	cid := strings.TrimSpace(docker(t, "create", agentImage()))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", cid).Run() })
	docker(t, "cp", cid+":/usr/local/bin/wal-g", tools+"/wal-g")

	prefix := "/backup/it"
	walg := "WALG_FILE_PREFIX=" + prefix + " /tools/wal-g"
	docker(t, "run", "-d", "--name", id, "-e", "POSTGRES_PASSWORD=pw",
		"-e", "WALG_FILE_PREFIX="+prefix,
		"-v", tools+":/tools:ro", "-v", backup+":/backup", image,
		"bash", "-c", "mkdir -p "+prefix+" && chown postgres "+prefix+" && exec docker-entrypoint.sh postgres -c wal_level=replica -c archive_mode=on "+
			"-c archive_command='/tools/wal-g wal-push %p'")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
	sh := func(cmd string) (string, error) {
		out, err := exec.Command("docker", "exec", "--user", "postgres", id, "bash", "-c", cmd).CombinedOutput()
		return string(out), err
	}
	waitFor(t, "the server to accept connections", func() bool {
		_, err := sh("pg_isready -h 127.0.0.1 -q")
		return err == nil
	})

	if out, err := sh("psql -XAtqc 'create table t(v int); insert into t select generate_series(1,1000); select pg_switch_wal()'"); err != nil {
		t.Fatalf("write and switch WAL: %v\n%s", err, out)
	}
	waitFor(t, "WAL archived to the file prefix", func() bool {
		out, _ := sh("psql -XAtqc 'select archived_count > 0 and failed_count = 0 from pg_stat_archiver'")
		return strings.TrimSpace(out) == "t"
	})
	if out, err := sh(walg + " backup-push \"$PGDATA\""); err != nil {
		t.Fatalf("backup-push to the file prefix: %v\n%s", err, out)
	}
	out, err := sh(walg + " backup-list --json 2>/dev/null")
	if err != nil {
		t.Fatalf("backup-list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "base_") {
		t.Fatalf("no base backup listed in the file prefix: %s", out)
	}
	if out, err := sh("ls " + prefix + "/wal_005 | head -1"); err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("no archived WAL in %s/wal_005: %v %s", prefix, err, out)
	}
}

// TestRestoreDrillFromAFileBackendToANamedPoint archives a server to a file
// prefix, then restores a second server from that prefix to a named restore
// point with the restore commands the agent writes, and checks only the rows
// written before the point are there. The restoring server's own storage is
// S3, so the source must replace it.
func TestRestoreDrillFromAFileBackendToANamedPoint(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	if exec.Command("docker", "image", "inspect", agentImage()).Run() != nil {
		t.Skipf("agent image %s not built", agentImage())
	}
	id := fmt.Sprintf("pgha-it-drill-%d", time.Now().UnixNano())
	base, err := os.MkdirTemp("", "pgha-it-drill-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "run", "--rm", "-v", base+":/b", "--entrypoint", "sh", image, "-c", "rm -rf /b/*").Run()
		_ = os.RemoveAll(base)
	})
	tools, backup := base+"/tools", base+"/backup"
	for _, d := range []string{tools, backup} {
		if err := os.Mkdir(d, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	cid := strings.TrimSpace(docker(t, "create", agentImage()))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", cid).Run() })
	docker(t, "cp", cid+":/usr/local/bin/wal-g", tools+"/wal-g")

	prefix := "/backup/source"
	src, dst := id+"-src", id+"-dst"
	docker(t, "run", "-d", "--name", src, "-e", "POSTGRES_PASSWORD=pw", "-e", "WALG_FILE_PREFIX="+prefix,
		"-v", tools+":/tools:ro", "-v", backup+":/backup", image,
		"bash", "-c", "mkdir -p "+prefix+" && chown postgres "+prefix+" && exec docker-entrypoint.sh postgres -c wal_level=replica -c archive_mode=on "+
			"-c archive_command='/tools/wal-g wal-push %p'")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", src).Run() })
	in := func(name, cmd string) string {
		t.Helper()
		out, err := exec.Command("docker", "exec", "--user", "postgres", name, "bash", "-c", cmd).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %s: %v\n%s", name, cmd, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	waitFor(t, "the source server", func() bool {
		return exec.Command("docker", "exec", src, "pg_isready", "-h", "127.0.0.1", "-q").Run() == nil
	})
	in(src, `psql -XAtqc "create table t(v int); insert into t values (1)"`)
	in(src, `/tools/wal-g backup-push "$PGDATA" 2>/dev/null`)
	// Separate statements: a multi-statement -c is one transaction, which
	// would commit 2 and 3 together after the point.
	in(src, `psql -XAtq -c "insert into t values (2)" -c "select pg_create_restore_point('drill')" -c "insert into t values (3)"`)
	// Wait for the segment the switch closes: later WAL (a checkpoint) lands
	// in the next segment, which is not archived until it fills.
	switched := in(src, `psql -XAtqc "select pg_walfile_name(pg_switch_wal())"`)
	waitFor(t, "the WAL after the restore point archived", func() bool {
		out, _ := exec.Command("docker", "exec", "--user", "postgres", src, "psql", "-XAtqc",
			"select coalesce(last_archived_wal >= '"+switched+"', false) from pg_stat_archiver").Output()
		return strings.TrimSpace(string(out)) == "t"
	})

	// The restoring server is configured for S3; the source must win.
	s := Source{Storage: StorageFile, Prefix: prefix}
	docker(t, "run", "-d", "--name", dst, "--user", "postgres", "-e", "WALG_S3_PREFIX=s3://nowhere/else",
		"-v", tools+":/tools:ro", "-v", backup+":/backup:ro", "--entrypoint", "sleep", image, "infinity")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", dst).Run() })
	names := in(dst, s.ShellPrefix()+"/tools/wal-g backup-list --json 2>/dev/null")
	got, err := backupNames(names)
	if err != nil || len(got) != 1 {
		t.Fatalf("source backups = %v, %v (%s)", got, err, names)
	}
	data := "/var/lib/postgresql/restored"
	in(dst, s.ShellPrefix()+"/tools/wal-g backup-fetch "+data+" "+got[0]+" 2>/dev/null")
	in(dst, "mkdir -p "+data+"/pg_wal/archive_status && chmod 700 "+data+" && touch "+data+"/recovery.signal")
	// Rendered exactly as the agent renders its settings.
	conf := RenderConf(map[string]string{
		"restore_command":        s.ShellPrefix() + `/tools/wal-g wal-fetch "%f" "%p"`,
		"recovery_target_name":   "drill",
		"recovery_target_action": "promote",
	})
	in(dst, "cat >> "+data+"/postgresql.auto.conf <<'CONF'\n"+string(conf)+"CONF")
	if out, err := exec.Command("docker", "exec", "--user", "postgres", dst, "pg_ctl", "-D", data, "-w", "-t", "60", "-l", "/tmp/restore.log", "start").CombinedOutput(); err != nil {
		log, _ := exec.Command("docker", "exec", dst, "cat", "/tmp/restore.log").CombinedOutput()
		t.Fatalf("start the restored server: %v\n%s\n%s", err, out, log)
	}
	waitFor(t, "the restored server to promote", func() bool {
		out, _ := exec.Command("docker", "exec", "--user", "postgres", dst, "psql", "-XAtqc", "select pg_is_in_recovery()").Output()
		return strings.TrimSpace(string(out)) == "f"
	})
	if rows := in(dst, `psql -XAtqc "select string_agg(v::text, ',' order by v) from t"`); rows != "1,2" {
		t.Fatalf("restored rows = %q, want 1,2 (the restore point is after 2 and before 3)", rows)
	}
}
