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

// Tests against a real PostgreSQL primary and standby in Docker, using the
// chart's pinned image. Run with: go test -tags integration ./internal/pg/

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	golog "github.com/Bugs5382/go-log"
)

const image = "docker.io/library/postgres:18.6-bookworm@sha256:3725f4e2499eef5134592b3b4ab79a543ed7f8e533b05b5b637af926630f6650"

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// pair starts a primary and a streaming standby that share a socket volume
// with the test, and returns a DB for each.
func pair(t *testing.T) (primary, standby *DB, standbyName string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	id := fmt.Sprintf("pgha-it-%d", time.Now().UnixNano())
	net := docker(t, "network", "create", id)
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", net).Run() })
	// Socket directories outside t.TempDir: the server's socket files belong
	// to the container's uid and cannot be removed by the test's cleanup.
	base, err := os.MkdirTemp("", "pgha-it-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "run", "--rm", "-v", base+":/b", "--entrypoint", "sh", image, "-c", "rm -rf /b/*").Run()
		_ = os.RemoveAll(base)
	})
	psock, ssock := base+"/p", base+"/s"
	for _, d := range []string{psock, ssock} {
		if err := os.Mkdir(d, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	run := func(name, sock string, cmd ...string) {
		args := append([]string{"run", "-d", "--name", name, "--network", id, "--network-alias", name,
			"-e", "POSTGRES_PASSWORD=pw", "-e", "POSTGRES_HOST_AUTH_METHOD=trust",
			"-v", sock + ":/var/run/postgresql", image}, cmd...)
		docker(t, args...)
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	}
	// The image's pg_hba only trusts replication from localhost; the test
	// network gets its own line.
	hba := "local all all trust\nhost all all all trust\nhost replication all all trust\n"
	run(id+"-p", psock, "bash", "-c", "printf '"+hba+"' > /tmp/hba && exec docker-entrypoint.sh postgres -c wal_level=replica -c hot_standby=on -c hba_file=/tmp/hba")
	p := NewDB(psock, golog.Nop())
	t.Cleanup(p.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// The image's entrypoint first runs a socket-only server for its init
	// step and then restarts with TCP; wait for the TCP listener, which is
	// what the standby clones from.
	deadline := time.Now().Add(60 * time.Second)
	for exec.Command("docker", "exec", id+"-p", "pg_isready", "-h", "127.0.0.1", "-q").Run() != nil {
		if time.Now().After(deadline) {
			t.Fatalf("primary never accepted TCP connections:\n%s", docker(t, "logs", id+"-p"))
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err := p.WaitReady(ctx, time.Second); err != nil {
		t.Fatalf("primary not ready: %v", err)
	}
	if err := p.Exec(ctx, "postgres", "SELECT pg_create_physical_replication_slot('s1', true)"); err != nil {
		t.Fatal(err)
	}
	standbyName = id + "-s"
	clone := fmt.Sprintf("for i in $(seq 1 30); do rm -rf \"$PGDATA\"; pg_basebackup -h %s-p -U postgres -D \"$PGDATA\" -X stream -R -S s1 -c fast && exec docker-entrypoint.sh postgres -c hot_standby=on; sleep 1; done; exit 1", id)
	args := []string{"run", "-d", "--name", standbyName, "--network", id, "-e", "POSTGRES_PASSWORD=pw",
		"-v", ssock + ":/var/run/postgresql", "--user", "postgres", "--entrypoint", "bash", image, "-c", clone}
	docker(t, args...)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", standbyName).Run() })
	s := NewDB(ssock, golog.Nop())
	t.Cleanup(s.Close)
	if err := s.WaitReady(ctx, time.Second); err != nil {
		t.Fatalf("standby not ready: %v\n%s", err, docker(t, "logs", standbyName))
	}
	return p, s, standbyName
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestStatusOnARealPrimaryAndStandby(t *testing.T) {
	p, s, _ := pair(t)
	ctx := context.Background()
	if err := p.Exec(ctx, "postgres", "CREATE TABLE it (v int); INSERT INTO it VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	ps, err := p.Status(ctx)
	if err != nil || ps.InRecovery || ps.CurrentLSN == 0 || ps.Timeline != 1 || ps.SystemID == "" {
		t.Fatalf("primary status = %+v, %v", ps, err)
	}
	waitFor(t, "the standby to stream", func() bool {
		ss, err := s.Status(ctx)
		return err == nil && ss.Streaming() && ss.ReceiveLSN >= ps.CurrentLSN
	})
	ss, _ := s.Status(ctx)
	if !ss.InRecovery || ss.SystemID != ps.SystemID || ss.Position() < ps.CurrentLSN || ss.Timeline != 1 {
		t.Fatalf("standby status = %+v", ss)
	}
	reps, err := p.Replicas(ctx)
	if err != nil || len(reps) != 1 || reps[0].State != "streaming" {
		t.Fatalf("replicas = %+v, %v", reps, err)
	}
	slots, err := p.Slots(ctx)
	if err != nil || len(slots) != 1 || slots[0].Name != "s1" || !slots[0].Active {
		t.Fatalf("slots = %+v, %v", slots, err)
	}
}

func TestPromoteARealStandby(t *testing.T) {
	_, s, _ := pair(t)
	ctx := context.Background()
	ok, err := s.Promote(ctx, 30*time.Second)
	if err != nil || !ok {
		t.Fatalf("promote = %v, %v", ok, err)
	}
	st, err := s.Status(ctx)
	if err != nil || st.InRecovery || st.Timeline != 2 {
		t.Fatalf("after promotion: %+v, %v", st, err)
	}
	if err := s.Exec(ctx, "postgres", "CREATE TABLE after_promote (v int)"); err != nil {
		t.Fatalf("promoted server does not take writes: %v", err)
	}
}

func TestRolesOnARealServer(t *testing.T) {
	p, _, _ := pair(t)
	ctx := context.Background()
	spec := Spec{
		Roles:     []RoleSpec{{Name: "app", PasswordFile: "x"}},
		Databases: []DatabaseSpec{{Name: "appdb", Owner: "app", Extensions: []string{"pgcrypto"}}},
		PgBouncer: true,
	}
	creds := Credentials{Superuser: "su", Replication: "rep", Rewind: "rw", PgBouncer: "pb"}
	read := func(string) (string, error) { return "apppw", nil }
	r := NewReconciler(p)
	for i := 0; i < 2; i++ {
		if err := r.Apply(ctx, spec, creds, read); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}
	got, err := p.QueryStrings(ctx, "postgres", "SELECT rolname::text FROM pg_roles WHERE rolname IN ('app','replicator','pgha_rewind','pgbouncer_auth') ORDER BY 1")
	if err != nil || strings.Join(got, ",") != "app,pgbouncer_auth,pgha_rewind,replicator" {
		t.Fatalf("roles = %v, %v", got, err)
	}
	v, err := p.QueryStrings(ctx, "postgres", "SELECT rolpassword FROM pg_authid WHERE rolname = 'app'")
	if err != nil || !ScramMatches(v[0], "apppw") {
		t.Fatalf("app password not stored as a matching SCRAM verifier: %v", err)
	}
	auth, err := p.QueryStrings(ctx, "postgres", "SELECT passwd FROM pgbouncer.get_auth('app')")
	if err != nil || len(auth) != 1 || !strings.HasPrefix(auth[0], "SCRAM-SHA-256$") {
		t.Fatalf("auth lookup = %v, %v", auth, err)
	}
	none, err := p.QueryStrings(ctx, "postgres", "SELECT passwd FROM pgbouncer.get_auth('postgres')")
	if err != nil || len(none) != 0 {
		t.Fatalf("auth lookup returned the superuser's verifier: %v, %v", none, err)
	}
	grants, err := p.QueryStrings(ctx, "postgres", "SELECT has_function_privilege('pgbouncer_auth', 'pgbouncer.get_auth(text)', 'EXECUTE')::text || ',' || has_function_privilege('app', 'pgbouncer.get_auth(text)', 'EXECUTE')::text")
	if err != nil || grants[0] != "true,false" {
		t.Fatalf("lookup function grants (pgbouncer_auth, app) = %v, %v", grants, err)
	}
	ext, err := p.QueryStrings(ctx, "appdb", "SELECT extname::text FROM pg_extension WHERE extname = 'pgcrypto'")
	if err != nil || len(ext) != 1 {
		t.Fatalf("extension = %v, %v", ext, err)
	}
}
