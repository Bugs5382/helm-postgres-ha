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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeExec records statements and answers the two lookups the reconciler runs.
type fakeExec struct {
	stmts     []string
	passwords map[string]string
	databases []string
}

func (f *fakeExec) Exec(_ context.Context, db, sql string, _ ...any) error {
	f.stmts = append(f.stmts, db+": "+sql)
	return nil
}

func (f *fakeExec) QueryStrings(_ context.Context, _ string, sql string, args ...any) ([]string, error) {
	if strings.Contains(sql, "pg_authid") {
		v, ok := f.passwords[args[0].(string)]
		if !ok {
			return nil, nil
		}
		return []string{v}, nil
	}
	return f.databases, nil
}

func (f *fakeExec) has(sub string) bool {
	for _, s := range f.stmts {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func spec() Spec {
	return Spec{
		Roles:     []RoleSpec{{Name: "app", PasswordFile: "app"}},
		Databases: []DatabaseSpec{{Name: "app", Owner: "app", Extensions: []string{"pgcrypto"}}},
		PgBouncer: true,
	}
}

func creds() Credentials {
	return Credentials{Superuser: "su", Replication: "rep", Rewind: "rw", PgBouncer: "pb"}
}

func readPW(string) (string, error) { return "apppw", nil }

func TestReconcileFreshServer(t *testing.T) {
	f := &fakeExec{passwords: map[string]string{"postgres": ""}, databases: []string{"postgres", "template1"}}
	if err := NewReconciler(f).Apply(context.Background(), spec(), creds(), readPW); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`CREATE ROLE "replicator"`,
		`ALTER ROLE "replicator" WITH LOGIN REPLICATION`,
		`CREATE ROLE "pgha_rewind"`,
		`GRANT EXECUTE ON FUNCTION pg_catalog.pg_read_binary_file(text, bigint, bigint, boolean) TO "pgha_rewind"`,
		`CREATE ROLE "app"`,
		`ALTER ROLE "app" WITH LOGIN NOSUPERUSER NOREPLICATION NOBYPASSRLS NOCREATEDB NOCREATEROLE CONNECTION LIMIT -1`,
		`CREATE ROLE "pgbouncer_auth"`,
		`CREATE OR REPLACE FUNCTION pgbouncer.get_auth`,
		`postgres: CREATE DATABASE "app" OWNER "app"`,
		`app: CREATE EXTENSION IF NOT EXISTS "pgcrypto"`,
		`ALTER ROLE "postgres" WITH PASSWORD 'SCRAM-SHA-256$4096:`,
	} {
		if !f.has(want) {
			t.Errorf("missing statement %q in\n%s", want, strings.Join(f.stmts, "\n"))
		}
	}
	for _, s := range f.stmts {
		for _, pw := range []string{"'su'", "'rep'", "'apppw'"} {
			if strings.Contains(s, pw) {
				t.Errorf("plain-text password in statement %q", s)
			}
		}
	}
}

func TestReconcileLeavesMatchingPasswords(t *testing.T) {
	v := func(pw string) string {
		s, err := ScramVerifier(pw)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	f := &fakeExec{
		passwords: map[string]string{"postgres": v("su"), "replicator": v("rep"), "pgha_rewind": v("rw"), "app": v("apppw"), "pgbouncer_auth": v("pb")},
		databases: []string{"postgres", "app"},
	}
	if err := NewReconciler(f).Apply(context.Background(), spec(), creds(), readPW); err != nil {
		t.Fatal(err)
	}
	if f.has("PASSWORD") {
		t.Fatalf("rewrote a matching password:\n%s", strings.Join(f.stmts, "\n"))
	}
	if f.has("CREATE ROLE") || f.has("CREATE DATABASE") {
		t.Fatal("recreated existing objects")
	}
	if !f.has(`ALTER DATABASE "app" OWNER TO "app"`) {
		t.Fatal("existing database owner not enforced")
	}
}

func TestReconcileRotatesChangedPassword(t *testing.T) {
	old, _ := ScramVerifier("old")
	f := &fakeExec{passwords: map[string]string{"postgres": old}, databases: []string{"postgres"}}
	if err := NewReconciler(f).Apply(context.Background(), Spec{}, creds(), readPW); err != nil {
		t.Fatal(err)
	}
	if !f.has(`ALTER ROLE "postgres" WITH PASSWORD`) {
		t.Fatal("changed superuser password not applied")
	}
}

func TestSpecValidate(t *testing.T) {
	for name, s := range map[string]Spec{
		"reserved":    {Roles: []RoleSpec{{Name: "replicator", PasswordFile: "x"}}},
		"pg prefix":   {Roles: []RoleSpec{{Name: "pg_monitor", PasswordFile: "x"}}},
		"twice":       {Roles: []RoleSpec{{Name: "a", PasswordFile: "x"}, {Name: "a", PasswordFile: "x"}}},
		"no password": {Roles: []RoleSpec{{Name: "a"}}},
		"bad owner":   {Databases: []DatabaseSpec{{Name: "d", Owner: "ghost"}}},
		"db twice":    {Databases: []DatabaseSpec{{Name: "d"}, {Name: "d"}}},
	} {
		if s.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := spec().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSpec(t *testing.T) {
	p := filepath.Join(t.TempDir(), "roles.json")
	if err := os.WriteFile(p, []byte(`{"roles":[{"name":"app","passwordFile":"/x","connectionLimit":5}],"databases":[{"name":"app","owner":"app"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSpec(p)
	if err != nil {
		t.Fatal(err)
	}
	if *s.Roles[0].ConnectionLimit != 5 || !strings.Contains(strings.Join(roleOptions(s.Roles[0]), " "), "CONNECTION LIMIT 5") {
		t.Fatalf("spec = %+v", s)
	}
}

func TestQuoting(t *testing.T) {
	if QuoteIdent(`a"b`) != `"a""b"` || QuoteLiteral("it's") != "'it''s'" {
		t.Fatal("quoting wrong")
	}
}

func TestParseControlAndVersion(t *testing.T) {
	c, err := parseControlData("pg_control version number:            1800\nDatabase system identifier:           7412345678901234567\nDatabase cluster state:               in production\nLatest checkpoint's TimeLineID:       3\n")
	if err != nil || c.SystemID != "7412345678901234567" || c.Timeline != 3 || c.State != "in production" {
		t.Fatalf("control = %+v %v", c, err)
	}
	if _, err := parseControlData("nothing"); err == nil {
		t.Fatal("empty control parsed")
	}
	if m, err := parseMajor("postgres (PostgreSQL) 18.6 (Debian 18.6-1.pgdg12+1)\n"); err != nil || m != 18 {
		t.Fatalf("major = %d %v", m, err)
	}
}
