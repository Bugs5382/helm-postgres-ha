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
	"context"
	"time"

	"github.com/Bugs5382/helm-postgres-ha/internal/lease"
	"github.com/Bugs5382/helm-postgres-ha/internal/peer"
	"github.com/Bugs5382/helm-postgres-ha/internal/pg"
)

// RejoinMarker is a file in the data directory meaning "this data last ran
// read-write, or may have diverged; rewind it before following a primary".
// It survives restarts, so a fenced primary never follows blindly.
const RejoinMarker = "pgha.rejoin"

// Node is the local PostgreSQL server and its data directory.
type Node interface {
	HasData() (bool, error)
	DataMajor() (int, error)
	ServerMajor(ctx context.Context) (int, error)
	Control(ctx context.Context) (pg.Control, error)
	HasSignal(name string) bool
	SetSignal(name string, on bool) error
	WriteAgentConf(settings map[string]string) (bool, error)
	WritePassFile(entries []pg.PassEntry) error

	Start() error
	Running() bool
	// LastFatal is the FATAL or PANIC message the last postmaster logged.
	LastFatal() string
	Stop(ctx context.Context, mode pg.StopMode) error
	Reload() error

	Ping(ctx context.Context) error
	Status(ctx context.Context) (pg.Status, error)
	Promote(ctx context.Context, wait time.Duration) (bool, error)
	// SwitchWAL closes the current WAL segment so it is archived now.
	SwitchWAL(ctx context.Context) error
	Slots(ctx context.Context) ([]pg.Slot, error)
	CreateSlot(ctx context.Context, name string) error
	DropSlot(ctx context.Context, name string) error
	Archiver(ctx context.Context) (pg.Archiver, error)
	Replicas(ctx context.Context) ([]pg.Replica, error)
	PendingRestart(ctx context.Context) ([]string, error)
	ApplyRoles(ctx context.Context, spec pg.Spec, creds pg.Credentials) error
	CloseConns()

	// Initdb creates a new cluster in the data directory.
	Initdb(ctx context.Context) error
	// Clone copies a running primary into the data directory and marks it a
	// standby.
	Clone(ctx context.Context, conninfo string) error
	// FetchBackup restores a base backup from src into the data directory.
	FetchBackup(ctx context.Context, src pg.Source, name string) error
	// SourceBackups lists the base backups in a restore source.
	SourceBackups(ctx context.Context, src pg.Source) ([]string, error)
	// ArchiveUsed reports whether the cluster's own WAL-G storage already
	// holds base backups or WAL.
	ArchiveUsed(ctx context.Context) (bool, error)
	// Rewind resynchronises the stopped data directory with a running
	// primary and reports whether anything had to change.
	Rewind(ctx context.Context, conninfo string) (bool, error)
	// MoveAside renames the data directory out of the way, keeping it.
	MoveAside(reason string) (string, error)
	// PrepareBackupPath creates the file backend's directory on the backup
	// volume; WAL-G needs it to exist.
	PrepareBackupPath(path string) error
	// BackupVolume reports the size and free space of the volume holding
	// path.
	BackupVolume(path string) (size, avail uint64, err error)
	// Disk reports the data volume's size, free space and pg_wal size.
	Disk() (size, avail, wal uint64, err error)
}

// Leases is the cluster Lease.
type Leases interface {
	Get(ctx context.Context) (lease.Record, error)
	Expired(r lease.Record) bool
	ObservedFor() time.Duration
	Acquire(ctx context.Context, r lease.Record, me string, ann map[string]string) (lease.Record, error)
	Renew(ctx context.Context, r lease.Record, me string, ann map[string]string) (lease.Record, error)
	Release(ctx context.Context, r lease.Record, me string, ann map[string]string) (lease.Record, error)
	Annotate(ctx context.Context, ann map[string]string) error
}

// Peers asks other members for their status.
type Peers interface {
	FetchAll(ctx context.Context, pods []string) map[string]peer.Result
}

// Kube reads and writes the role labels the Services select on.
type Kube interface {
	Role(ctx context.Context, pod string) (string, error)
	SetRole(ctx context.Context, pod, role string) error
}

// BackupRole tells the backup scheduler what this member is.
type BackupRole struct {
	Primary          bool
	StreamingStandby bool
}

// Poolers drops every PgBouncer's connections after a promotion; nil when
// PgBouncer is off.
type Poolers interface {
	Reset(ctx context.Context) (int, error)
}

// Backups is the scheduled base backup runner; nil when backups are off.
type Backups interface {
	Tick(ctx context.Context, role BackupRole)
	RequestNow()
	LastSuccess() time.Time
}
