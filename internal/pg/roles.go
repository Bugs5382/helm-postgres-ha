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
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

// Built-in roles the agent manages.
const (
	RoleSuperuser   = "postgres"
	RoleReplication = "replicator"
	RoleRewind      = "pgha_rewind"
	RolePgBouncer   = "pgbouncer_auth"
)

// RoleSpec is one application role from the chart's values.
type RoleSpec struct {
	Name            string   `json:"name"`
	PasswordFile    string   `json:"passwordFile"`
	CreateDB        bool     `json:"createdb,omitempty"`
	CreateRole      bool     `json:"createrole,omitempty"`
	ConnectionLimit *int     `json:"connectionLimit,omitempty"`
	InRoles         []string `json:"inRoles,omitempty"`
}

// DatabaseSpec is one database from the chart's values.
type DatabaseSpec struct {
	Name       string   `json:"name"`
	Owner      string   `json:"owner"`
	Extensions []string `json:"extensions,omitempty"`
}

// Spec is the whole role and database spec the chart renders to roles.json.
type Spec struct {
	Roles     []RoleSpec     `json:"roles"`
	Databases []DatabaseSpec `json:"databases"`
	// PgBouncer turns on the auth_query role and lookup function.
	PgBouncer bool `json:"pgbouncer,omitempty"`
}

// LoadSpec reads roles.json.
func LoadSpec(path string) (Spec, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- chart-mounted spec path
	if err != nil {
		return Spec{}, err
	}
	var s Spec
	if err := json.Unmarshal(b, &s); err != nil {
		return Spec{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, s.Validate()
}

var reserved = []string{RoleSuperuser, RoleReplication, RoleRewind, RolePgBouncer}

// Validate rejects specs the reconciler would refuse halfway through.
func (s Spec) Validate() error {
	seen := map[string]bool{}
	for _, r := range s.Roles {
		switch {
		case r.Name == "":
			return fmt.Errorf("a role has no name")
		case slices.Contains(reserved, r.Name) || strings.HasPrefix(r.Name, "pg_"):
			return fmt.Errorf("role %q is reserved", r.Name)
		case seen[r.Name]:
			return fmt.Errorf("role %q is listed twice", r.Name)
		case r.PasswordFile == "":
			return fmt.Errorf("role %q has no password file", r.Name)
		}
		seen[r.Name] = true
	}
	dbs := map[string]bool{}
	for _, d := range s.Databases {
		switch {
		case d.Name == "":
			return fmt.Errorf("a database has no name")
		case dbs[d.Name]:
			return fmt.Errorf("database %q is listed twice", d.Name)
		case d.Owner != "" && d.Owner != RoleSuperuser && !seen[d.Owner]:
			return fmt.Errorf("database %q is owned by %q, which is not a listed role", d.Name, d.Owner)
		}
		dbs[d.Name] = true
	}
	return nil
}

// Execer is the slice of DB the reconciler needs.
type Execer interface {
	Exec(ctx context.Context, dbname, sql string, args ...any) error
	QueryStrings(ctx context.Context, dbname, sql string, args ...any) ([]string, error)
}

// Credentials are the passwords for the built-in roles.
type Credentials struct {
	Superuser   string
	Replication string
	Rewind      string
	PgBouncer   string
}

// QuoteIdent quotes an SQL identifier.
func QuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// QuoteLiteral quotes an SQL string literal.
func QuoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// Reconciler creates and updates roles and databases on the primary. Every
// step is idempotent, so it runs again on each promotion and whenever the spec
// or a password changes.
type Reconciler struct {
	db Execer
}

// NewReconciler returns a Reconciler.
func NewReconciler(db Execer) *Reconciler { return &Reconciler{db: db} }

// Apply makes the server match the spec and credentials.
func (r *Reconciler) Apply(ctx context.Context, spec Spec, creds Credentials, readPassword func(string) (string, error)) error {
	if err := r.ensureRole(ctx, RoleSuperuser, creds.Superuser, nil); err != nil {
		return err
	}
	if err := r.ensureRole(ctx, RoleReplication, creds.Replication, []string{"LOGIN", "REPLICATION"}); err != nil {
		return err
	}
	if err := r.ensureRole(ctx, RoleRewind, creds.Rewind, []string{"LOGIN"}); err != nil {
		return err
	}
	for _, fn := range []string{
		"pg_catalog.pg_ls_dir(text, boolean, boolean)",
		"pg_catalog.pg_stat_file(text, boolean)",
		"pg_catalog.pg_read_binary_file(text)",
		"pg_catalog.pg_read_binary_file(text, bigint, bigint, boolean)",
	} {
		if err := r.db.Exec(ctx, "postgres", "GRANT EXECUTE ON FUNCTION "+fn+" TO "+QuoteIdent(RoleRewind)); err != nil {
			return fmt.Errorf("grant rewind functions: %w", err)
		}
	}
	for _, role := range spec.Roles {
		pw, err := readPassword(role.PasswordFile)
		if err != nil {
			return fmt.Errorf("password for role %q: %w", role.Name, err)
		}
		if err := r.ensureRole(ctx, role.Name, pw, roleOptions(role)); err != nil {
			return err
		}
		for _, in := range role.InRoles {
			if err := r.db.Exec(ctx, "postgres", "GRANT "+QuoteIdent(in)+" TO "+QuoteIdent(role.Name)); err != nil {
				return fmt.Errorf("grant %q to %q: %w", in, role.Name, err)
			}
		}
	}
	if spec.PgBouncer {
		if err := r.ensurePgBouncer(ctx, creds.PgBouncer); err != nil {
			return err
		}
	}
	return r.ensureDatabases(ctx, spec.Databases)
}

func roleOptions(r RoleSpec) []string {
	opts := []string{"LOGIN", "NOSUPERUSER", "NOREPLICATION", "NOBYPASSRLS"}
	if r.CreateDB {
		opts = append(opts, "CREATEDB")
	} else {
		opts = append(opts, "NOCREATEDB")
	}
	if r.CreateRole {
		opts = append(opts, "CREATEROLE")
	} else {
		opts = append(opts, "NOCREATEROLE")
	}
	limit := -1
	if r.ConnectionLimit != nil {
		limit = *r.ConnectionLimit
	}
	return append(opts, "CONNECTION LIMIT "+strconv.Itoa(limit))
}

// ensureRole creates a role when it is missing, applies its options, and sets
// its password only when the stored verifier does not match.
func (r *Reconciler) ensureRole(ctx context.Context, name, password string, opts []string) error {
	stored, err := r.db.QueryStrings(ctx, "postgres", "SELECT rolpassword FROM pg_authid WHERE rolname = $1", name)
	if err != nil {
		return fmt.Errorf("look up role %q: %w", name, err)
	}
	if len(stored) == 0 {
		if err := r.db.Exec(ctx, "postgres", "CREATE ROLE "+QuoteIdent(name)); err != nil {
			return fmt.Errorf("create role %q: %w", name, err)
		}
		stored = []string{""}
	}
	if len(opts) > 0 {
		if err := r.db.Exec(ctx, "postgres", "ALTER ROLE "+QuoteIdent(name)+" WITH "+strings.Join(opts, " ")); err != nil {
			return fmt.Errorf("set options on role %q: %w", name, err)
		}
	}
	if password == "" || ScramMatches(stored[0], password) {
		return nil
	}
	v, err := ScramVerifier(password)
	if err != nil {
		return err
	}
	if err := r.db.Exec(ctx, "postgres", "ALTER ROLE "+QuoteIdent(name)+" WITH PASSWORD "+QuoteLiteral(v)); err != nil {
		return fmt.Errorf("set password on role %q: %w", name, err)
	}
	return nil
}

// pgbouncerAuthSQL creates the lookup function PgBouncer's auth_query calls.
// It runs as its owner (a superuser) and returns verifiers only for ordinary
// login roles, never for superusers or the built-in roles.
const pgbouncerAuthSQL = `CREATE OR REPLACE FUNCTION pgbouncer.get_auth(p_usename text)
RETURNS TABLE(usename name, passwd text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
  SELECT rolname, rolpassword FROM pg_authid
  WHERE rolname = p_usename AND rolcanlogin AND NOT rolsuper AND NOT rolreplication
    AND rolname NOT IN ('pgha_rewind', 'pgbouncer_auth')
    AND (rolvaliduntil IS NULL OR rolvaliduntil > now())
$$`

func (r *Reconciler) ensurePgBouncer(ctx context.Context, password string) error {
	if err := r.ensureRole(ctx, RolePgBouncer, password, []string{"LOGIN", "NOSUPERUSER", "NOCREATEDB", "NOCREATEROLE"}); err != nil {
		return err
	}
	for _, stmt := range []string{
		"CREATE SCHEMA IF NOT EXISTS pgbouncer",
		"REVOKE ALL ON SCHEMA pgbouncer FROM PUBLIC",
		pgbouncerAuthSQL,
		"REVOKE ALL ON FUNCTION pgbouncer.get_auth(text) FROM PUBLIC",
		"GRANT USAGE ON SCHEMA pgbouncer TO " + QuoteIdent(RolePgBouncer),
		"GRANT EXECUTE ON FUNCTION pgbouncer.get_auth(text) TO " + QuoteIdent(RolePgBouncer),
	} {
		if err := r.db.Exec(ctx, "postgres", stmt); err != nil {
			return fmt.Errorf("set up the pgbouncer lookup: %w", err)
		}
	}
	return nil
}

func (r *Reconciler) ensureDatabases(ctx context.Context, dbs []DatabaseSpec) error {
	existing, err := r.db.QueryStrings(ctx, "postgres", "SELECT datname::text FROM pg_database")
	if err != nil {
		return fmt.Errorf("list databases: %w", err)
	}
	for _, d := range dbs {
		owner := d.Owner
		if owner == "" {
			owner = RoleSuperuser
		}
		if slices.Contains(existing, d.Name) {
			if err := r.db.Exec(ctx, "postgres", "ALTER DATABASE "+QuoteIdent(d.Name)+" OWNER TO "+QuoteIdent(owner)); err != nil {
				return fmt.Errorf("set owner of database %q: %w", d.Name, err)
			}
		} else if err := r.db.Exec(ctx, "postgres", "CREATE DATABASE "+QuoteIdent(d.Name)+" OWNER "+QuoteIdent(owner)); err != nil {
			return fmt.Errorf("create database %q: %w", d.Name, err)
		}
		for _, ext := range d.Extensions {
			if err := r.db.Exec(ctx, d.Name, "CREATE EXTENSION IF NOT EXISTS "+QuoteIdent(ext)); err != nil {
				return fmt.Errorf("create extension %q in %q: %w", ext, d.Name, err)
			}
		}
	}
	return nil
}
