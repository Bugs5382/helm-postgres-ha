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
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	golog "github.com/Bugs5382/go-log"
	postgres "github.com/Bugs5382/go-postgres"

	"github.com/Bugs5382/helm-postgres-ha/internal/proc"
)

// LocalConfig locates the local server's files.
type LocalConfig struct {
	DataDir               string
	ConfigFile            string
	HBAFile               string
	SocketDir             string
	PassFile              string
	SuperuserPasswordFile string
	WalG                  string
	InitdbArgs            []string
	// Env is the environment of every child process (postgres, the server
	// tools and wal-g).
	Env []string
}

// Local is the local server: the postmaster, its data directory, the server
// tools and the SQL connection, behind one type.
type Local struct {
	cfg    LocalConfig
	log    golog.Logger
	runner *proc.Runner
	srv    *Server
	db     *DB
	tools  *Tools
}

// NewLocal finds the postgres binary on PATH and returns a Local.
func NewLocal(runner *proc.Runner, log golog.Logger, cfg LocalConfig, dbOpts ...postgres.Option) (*Local, error) {
	bin, err := exec.LookPath("postgres")
	if err != nil {
		return nil, fmt.Errorf("find postgres: %w", err)
	}
	return &Local{
		cfg:    cfg,
		log:    log,
		runner: runner,
		srv:    NewServer(runner, log, bin, cfg.DataDir, cfg.ConfigFile, cfg.HBAFile, cfg.Env),
		db:     NewDB(cfg.SocketDir, log, dbOpts...),
		tools:  NewTools(runner, log, cfg.Env),
	}, nil
}

// HasData reports whether the data directory is initialised.
func (l *Local) HasData() (bool, error) { return HasData(l.cfg.DataDir) }

// DataMajor returns the data directory's major version.
func (l *Local) DataMajor() (int, error) { return DataMajor(l.cfg.DataDir) }

// ServerMajor returns the installed server's major version.
func (l *Local) ServerMajor(ctx context.Context) (int, error) { return l.tools.ServerMajor(ctx) }

// Control reads pg_controldata.
func (l *Local) Control(ctx context.Context) (Control, error) {
	return l.tools.ControlData(ctx, l.cfg.DataDir)
}

// HasSignal reports whether a signal file exists in the data directory.
func (l *Local) HasSignal(name string) bool { return HasSignal(l.cfg.DataDir, name) }

// SetSignal creates or removes a signal file.
func (l *Local) SetSignal(name string, on bool) error { return SetSignal(l.cfg.DataDir, name, on) }

// WriteAgentConf writes the agent-owned settings.
func (l *Local) WriteAgentConf(settings map[string]string) (bool, error) {
	return WriteAgentConf(l.cfg.DataDir, settings)
}

// WritePassFile writes the libpq password file.
func (l *Local) WritePassFile(entries []PassEntry) error {
	if err := os.MkdirAll(filepath.Dir(l.cfg.PassFile), 0o700); err != nil {
		return err
	}
	_, err := WriteFileAtomic(l.cfg.PassFile, RenderPassFile(entries), 0o600)
	return err
}

// Start starts the postmaster.
func (l *Local) Start() error { return l.srv.Start() }

// Running reports whether the postmaster runs.
func (l *Local) Running() bool { return l.srv.Running() }

// LastFatal returns the FATAL or PANIC message the last postmaster logged.
func (l *Local) LastFatal() string { return l.srv.LastFatal() }

// Stop stops the postmaster.
func (l *Local) Stop(ctx context.Context, mode StopMode) error {
	defer l.db.Close()
	return l.srv.Stop(ctx, mode)
}

// Reload signals the postmaster to re-read its files.
func (l *Local) Reload() error { return l.srv.Reload() }

// Ping checks the server accepts connections.
func (l *Local) Ping(ctx context.Context) error { return l.db.Ping(ctx) }

// Status reads the replication state.
func (l *Local) Status(ctx context.Context) (Status, error) {
	if !l.srv.Running() {
		return Status{}, ErrNotRunning
	}
	return l.db.Status(ctx)
}

// Promote ends recovery.
func (l *Local) Promote(ctx context.Context, wait time.Duration) (bool, error) {
	return l.db.Promote(ctx, wait)
}

// Slots lists replication slots.
func (l *Local) Slots(ctx context.Context) ([]Slot, error) { return l.db.Slots(ctx) }

// CreateSlot creates a replication slot.
func (l *Local) CreateSlot(ctx context.Context, name string) error { return l.db.CreateSlot(ctx, name) }

// DropSlot drops a replication slot.
func (l *Local) DropSlot(ctx context.Context, name string) error { return l.db.DropSlot(ctx, name) }

// Archiver reads the archiver counters.
func (l *Local) Archiver(ctx context.Context) (Archiver, error) { return l.db.Archiver(ctx) }

// Replicas reads pg_stat_replication.
func (l *Local) Replicas(ctx context.Context) ([]Replica, error) { return l.db.Replicas(ctx) }

// PendingRestart lists settings waiting for a restart.
func (l *Local) PendingRestart(ctx context.Context) ([]string, error) {
	return l.db.PendingRestart(ctx)
}

// ApplyRoles reconciles roles and databases.
func (l *Local) ApplyRoles(ctx context.Context, spec Spec, creds Credentials) error {
	return NewReconciler(l.db).Apply(ctx, spec, creds, ReadSecret)
}

// CloseConns drops pooled connections.
func (l *Local) CloseConns() { l.db.Close() }

// scratch returns an empty sibling directory of the data directory. Building
// new data next to the real path and renaming it in means a half-finished
// initdb, clone or restore is never mistaken for a data directory.
func (l *Local) scratch(suffix string) (string, error) {
	dir := l.cfg.DataDir + "." + suffix
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func (l *Local) install(dir string) error {
	if err := os.Remove(l.cfg.DataDir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("data directory %s is not empty: %w", l.cfg.DataDir, err)
	}
	return os.Rename(dir, l.cfg.DataDir)
}

// Initdb creates a new cluster.
func (l *Local) Initdb(ctx context.Context) error {
	dir, err := l.scratch("init")
	if err != nil {
		return err
	}
	if err := l.tools.Initdb(ctx, dir, l.cfg.SuperuserPasswordFile, l.cfg.InitdbArgs); err != nil {
		return err
	}
	return l.install(dir)
}

// Clone copies a running primary.
func (l *Local) Clone(ctx context.Context, conninfo string) error {
	dir, err := l.scratch("clone")
	if err != nil {
		return err
	}
	if err := l.tools.BaseBackup(ctx, dir, conninfo); err != nil {
		return err
	}
	return l.install(dir)
}

// FetchBackup restores a WAL-G base backup from prefix (the cluster's own
// storage when empty).
func (l *Local) FetchBackup(ctx context.Context, prefix, name string) error {
	dir, err := l.scratch("restore")
	if err != nil {
		return err
	}
	env := l.cfg.Env
	if prefix != "" {
		env = append(append([]string{}, env...), "WALG_S3_PREFIX="+prefix)
	}
	if _, err := l.runner.Run(ctx, env, l.cfg.WalG, "backup-fetch", dir, name); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "pg_wal", "archive_status"), 0o700); err != nil {
		return err
	}
	return l.install(dir)
}

// PrepareBackupPath creates the file backend's directory.
func (l *Local) PrepareBackupPath(path string) error {
	return os.MkdirAll(path, 0o700)
}

// BackupVolume reports the size and free space of the volume holding path.
func (l *Local) BackupVolume(path string) (uint64, uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize) // #nosec G115 -- block size is positive
	return st.Blocks * bs, st.Bavail * bs, nil
}

// Rewind runs pg_rewind against a primary.
func (l *Local) Rewind(ctx context.Context, conninfo string) (bool, error) {
	return l.tools.Rewind(ctx, l.cfg.DataDir, conninfo, l.cfg.ConfigFile)
}

// MoveAside renames the data directory, keeping it for an operator.
func (l *Local) MoveAside(reason string) (string, error) {
	dest := l.cfg.DataDir + ".aside-" + reason + "-" + time.Now().UTC().Format("20060102T150405Z")
	if err := os.Rename(l.cfg.DataDir, dest); err != nil {
		return "", err
	}
	l.log.Warn("data directory moved aside", golog.F("path", dest))
	return dest, nil
}

// Disk reports the data volume's size, free space and pg_wal size.
func (l *Local) Disk() (uint64, uint64, uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(l.cfg.DataDir), &st); err != nil {
		return 0, 0, 0, err
	}
	bs := uint64(st.Bsize) // #nosec G115 -- block size is positive
	var wal uint64
	_ = filepath.WalkDir(filepath.Join(l.cfg.DataDir, "pg_wal"), func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && info.Mode().IsRegular() {
			wal += uint64(info.Size()) // #nosec G115 -- file sizes are non-negative
		}
		return nil
	})
	return st.Blocks * bs, st.Bavail * bs, wal, nil
}
