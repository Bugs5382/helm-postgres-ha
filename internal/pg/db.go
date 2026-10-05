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
	"sync"
	"time"

	golog "github.com/Bugs5382/go-log"
	postgres "github.com/Bugs5382/go-postgres"
)

// DB runs the agent's SQL on the local server over the Unix socket, as the
// postgres superuser through peer authentication. No password crosses the
// network for the agent's own work.
type DB struct {
	socketDir string
	log       golog.Logger
	opts      []postgres.Option

	mu    sync.Mutex
	pools map[string]*postgres.DB
}

// NewDB returns a DB for the server listening in socketDir.
func NewDB(socketDir string, log golog.Logger, opts ...postgres.Option) *DB {
	base := []postgres.Option{
		postgres.WithMaxConns(4),
		postgres.WithConnectTimeout(3 * time.Second),
		postgres.WithHealthCheckPeriod(10 * time.Second),
		postgres.WithMaxConnIdleTime(time.Minute),
	}
	return &DB{socketDir: socketDir, log: log, opts: append(base, opts...), pools: map[string]*postgres.DB{}}
}

func (d *DB) dsn(dbname string) string {
	// The agent's own writes never wait for a synchronous standby: a primary
	// with no standby attached must still be able to create the roles the
	// standbys need to attach.
	return fmt.Sprintf("host=%s port=5432 user=postgres dbname=%s sslmode=disable application_name=pgha options='-c synchronous_commit=local'", d.socketDir, dbname)
}

// conn returns the pool for one database, connecting on first use.
func (d *DB) conn(ctx context.Context, dbname string) (*postgres.DB, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if p, ok := d.pools[dbname]; ok {
		return p, nil
	}
	p, err := postgres.New(ctx, d.dsn(dbname), d.opts...)
	if err != nil {
		return nil, err
	}
	d.pools[dbname] = p
	return p, nil
}

// Close drops every pool. Call it after the server restarts so no stale
// connection is reused.
func (d *DB) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, p := range d.pools {
		p.Close()
		delete(d.pools, k)
	}
}

// Ping reports whether the server accepts connections.
func (d *DB) Ping(ctx context.Context) error {
	p, err := d.conn(ctx, "postgres")
	if err != nil {
		return err
	}
	return p.Ping(ctx)
}

// Status is the local server's replication state.
type Status struct {
	InRecovery bool
	SystemID   string
	Timeline   int
	// CurrentLSN is the write position on a primary.
	CurrentLSN LSN
	// ReceiveLSN and ReplayLSN are a standby's positions.
	ReceiveLSN LSN
	ReplayLSN  LSN
	// Receiver is the WAL receiver's status (streaming, catchup, ...), empty
	// when no receiver runs.
	Receiver   string
	SenderHost string
}

// Streaming reports whether the standby is attached to an upstream.
func (s Status) Streaming() bool { return s.Receiver == "streaming" }

// Position is the most complete WAL position the server has: the write
// position on a primary, the received (or else replayed) one on a standby.
func (s Status) Position() LSN {
	if !s.InRecovery {
		return s.CurrentLSN
	}
	if s.ReceiveLSN > s.ReplayLSN {
		return s.ReceiveLSN
	}
	return s.ReplayLSN
}

const statusSQL = `
SELECT pg_is_in_recovery(),
       (SELECT system_identifier::text FROM pg_control_system()),
       (SELECT timeline_id FROM pg_control_checkpoint()),
       CASE WHEN pg_is_in_recovery() THEN ''
            ELSE pg_current_wal_lsn()::text END,
       CASE WHEN pg_is_in_recovery() THEN 0
            ELSE ('x' || substr(pg_walfile_name(pg_current_wal_lsn()), 1, 8))::bit(32)::int END,
       COALESCE(pg_last_wal_receive_lsn()::text, ''),
       COALESCE(pg_last_wal_replay_lsn()::text, ''),
       COALESCE((SELECT status FROM pg_stat_wal_receiver), ''),
       COALESCE((SELECT received_tli FROM pg_stat_wal_receiver), 0),
       COALESCE((SELECT sender_host FROM pg_stat_wal_receiver), '')`

// Status reads the local server's replication state.
func (d *DB) Status(ctx context.Context) (Status, error) {
	p, err := d.conn(ctx, "postgres")
	if err != nil {
		return Status{}, err
	}
	var (
		s                      Status
		ckptTLI, walTLI, rcvTL int
		cur, rcv, rep          string
	)
	err = p.Querier().QueryRow(ctx, statusSQL).Scan(&s.InRecovery, &s.SystemID, &ckptTLI, &cur, &walTLI, &rcv, &rep, &s.Receiver, &rcvTL, &s.SenderHost)
	if err != nil {
		return Status{}, fmt.Errorf("read status: %w", err)
	}
	if s.CurrentLSN, err = ParseLSN(cur); err != nil {
		return Status{}, err
	}
	if s.ReceiveLSN, err = ParseLSN(rcv); err != nil {
		return Status{}, err
	}
	if s.ReplayLSN, err = ParseLSN(rep); err != nil {
		return Status{}, err
	}
	s.Timeline = max(ckptTLI, walTLI, rcvTL)
	return s, nil
}

// Promote ends recovery and waits up to wait for the server to accept writes.
// It reports whether the promotion finished in time.
func (d *DB) Promote(ctx context.Context, wait time.Duration) (bool, error) {
	p, err := d.conn(ctx, "postgres")
	if err != nil {
		return false, err
	}
	var ok bool
	if err := p.Querier().QueryRow(ctx, "SELECT pg_promote(true, $1)", int(wait.Seconds())).Scan(&ok); err != nil {
		return false, fmt.Errorf("pg_promote: %w", err)
	}
	return ok, nil
}

// Slot is a physical replication slot.
type Slot struct {
	Name   string
	Active bool
	// Lost means the WAL the slot reserved was removed (past
	// max_slot_wal_keep_size); its standby can no longer stream through it.
	Lost bool
}

// Slots lists the physical replication slots.
func (d *DB) Slots(ctx context.Context) ([]Slot, error) {
	p, err := d.conn(ctx, "postgres")
	if err != nil {
		return nil, err
	}
	rows, err := p.Querier().Query(ctx, "SELECT slot_name::text, active, COALESCE(wal_status = 'lost', false) FROM pg_replication_slots WHERE slot_type = 'physical'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Slot
	for rows.Next() {
		var s Slot
		if err := rows.Scan(&s.Name, &s.Active, &s.Lost); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// CreateSlot creates a physical slot that reserves WAL immediately.
func (d *DB) CreateSlot(ctx context.Context, name string) error {
	p, err := d.conn(ctx, "postgres")
	if err != nil {
		return err
	}
	_, err = p.Querier().Exec(ctx, "SELECT pg_create_physical_replication_slot($1, true)", name)
	return err
}

// DropSlot drops a slot.
func (d *DB) DropSlot(ctx context.Context, name string) error {
	p, err := d.conn(ctx, "postgres")
	if err != nil {
		return err
	}
	_, err = p.Querier().Exec(ctx, "SELECT pg_drop_replication_slot($1)", name)
	return err
}

// Archiver is the WAL archiver's counters.
type Archiver struct {
	Archived     int64
	Failed       int64
	LastArchived time.Time
	LastFailed   time.Time
}

// Archiver reads pg_stat_archiver.
func (d *DB) Archiver(ctx context.Context) (Archiver, error) {
	p, err := d.conn(ctx, "postgres")
	if err != nil {
		return Archiver{}, err
	}
	var a Archiver
	var la, lf *time.Time
	err = p.Querier().QueryRow(ctx, "SELECT archived_count, failed_count, last_archived_time, last_failed_time FROM pg_stat_archiver").Scan(&a.Archived, &a.Failed, &la, &lf)
	if err != nil {
		return Archiver{}, err
	}
	if la != nil {
		a.LastArchived = *la
	}
	if lf != nil {
		a.LastFailed = *lf
	}
	return a, nil
}

// Replica is one row of pg_stat_replication.
type Replica struct {
	Name      string
	State     string
	SyncState string
	ReplayLag int64
}

// Replicas reads pg_stat_replication on a primary.
func (d *DB) Replicas(ctx context.Context) ([]Replica, error) {
	p, err := d.conn(ctx, "postgres")
	if err != nil {
		return nil, err
	}
	rows, err := p.Querier().Query(ctx, `SELECT application_name, state, sync_state,
		COALESCE(pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn), 0)::bigint FROM pg_stat_replication`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Replica
	for rows.Next() {
		var r Replica
		if err := rows.Scan(&r.Name, &r.State, &r.SyncState, &r.ReplayLag); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PendingRestart lists settings changed in the files that need a restart.
func (d *DB) PendingRestart(ctx context.Context) ([]string, error) {
	p, err := d.conn(ctx, "postgres")
	if err != nil {
		return nil, err
	}
	rows, err := p.Querier().Query(ctx, "SELECT name FROM pg_settings WHERE pending_restart ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Exec runs one statement in a database.
func (d *DB) Exec(ctx context.Context, dbname, sql string, args ...any) error {
	p, err := d.conn(ctx, dbname)
	if err != nil {
		return err
	}
	_, err = p.Querier().Exec(ctx, sql, args...)
	return err
}

// QueryStrings runs a query that returns one text column.
func (d *DB) QueryStrings(ctx context.Context, dbname, sql string, args ...any) ([]string, error) {
	p, err := d.conn(ctx, dbname)
	if err != nil {
		return nil, err
	}
	rows, err := p.Querier().Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s *string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		if s == nil {
			out = append(out, "")
		} else {
			out = append(out, *s)
		}
	}
	return out, rows.Err()
}

// WaitReady polls until the server accepts connections or ctx ends.
func (d *DB) WaitReady(ctx context.Context, every time.Duration) error {
	for {
		pctx, cancel := context.WithTimeout(ctx, every)
		err := d.Ping(pctx)
		cancel()
		if err == nil {
			return nil
		}
		d.Close()
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), err)
		case <-time.After(every):
		}
	}
}
