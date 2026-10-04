// Package config loads the agent's settings from the environment the chart
// renders, and validates them once at start-up.
package config

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
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Bugs5382/helm-postgres-ha/internal/cluster"
)

// Bootstrap modes for a cluster that has no data yet.
const (
	BootstrapInitdb  = "initdb"
	BootstrapRestore = "restore"
)

// Backup sources: which member takes the scheduled base backup.
const (
	BackupFromPrimary = "primary"
	BackupFromStandby = "standby"
)

// Synchronous replication modes.
const (
	SyncQuorum = "quorum"
	SyncStrict = "strict"
	SyncOff    = "off"
)

// Restore describes the backup a new cluster is restored from.
type Restore struct {
	// Backup is a WAL-G backup name, or LATEST.
	Backup string
	// Prefix is the WAL-G storage prefix holding the source backups. Empty means
	// the cluster's own prefix.
	Prefix string
	// At most one of the targets is set; none means recover to the end of WAL.
	TargetTime string
	TargetLSN  string
	TargetName string
	// TargetExclusive stops just before the target instead of just after it.
	TargetExclusive bool
}

// Bootstrap describes how an empty cluster gets its first data.
type Bootstrap struct {
	Mode       string
	InitdbArgs []string
	Restore    Restore
}

// Backup describes scheduled WAL-G base backups.
type Backup struct {
	Enabled bool
	// Schedule is a five-field cron expression, evaluated in UTC.
	Schedule string
	// Source is primary or standby.
	Source string
	// RetainFull is the minimum number of full backups kept.
	RetainFull int
	// RetainDays keeps every backup newer than this many days as well. Zero
	// keeps only RetainFull backups.
	RetainDays int
	// Timeout bounds one backup run.
	Timeout time.Duration
}

// Config is the agent's whole configuration.
type Config struct {
	Topology cluster.Topology
	// PodName is this member.
	PodName string
	// Revision is this pod's StatefulSet controller revision.
	Revision string

	Lease       string
	BackupLease string

	DataDir    string
	ConfigDir  string
	SecretsDir string
	TLSDir     string
	RunDir     string
	BinDir     string
	SocketDir  string

	HTTPAddr string
	PeerAddr string

	LeaseDuration   time.Duration
	RenewDeadline   time.Duration
	RetryPeriod     time.Duration
	RejoinTimeout   time.Duration
	PromoteTimeout  time.Duration
	PeerTimeout     time.Duration
	ShutdownTimeout time.Duration
	// SyncMode is quorum (commits wait for SyncCount of the streaming
	// standbys, or for none while no standby streams), strict (commits wait
	// even when no standby streams) or off.
	SyncMode  string
	SyncCount int
	// MaxLagOnFailover is the most WAL, in bytes, a standby may be behind the
	// last position the primary recorded and still be promoted.
	MaxLagOnFailover uint64
	// ReadyMaxLag is the replay lag, in bytes, past which a standby reports not
	// ready. Zero turns the check off.
	ReadyMaxLag uint64

	Bootstrap Bootstrap
	Backup    Backup

	// PgBouncer names the PgBouncer Service whose pods are told to reconnect
	// after a promotion. Empty when PgBouncer is off.
	PgBouncerService string

	OTLPEndpoint string
}

// ConfigFile is the postgresql.conf the chart mounts.
func (c Config) ConfigFile() string { return filepath.Join(c.ConfigDir, "postgresql.conf") }

// HBAFile is the pg_hba.conf the chart mounts.
func (c Config) HBAFile() string { return filepath.Join(c.ConfigDir, "pg_hba.conf") }

// RolesFile is the JSON role and database spec the chart mounts.
func (c Config) RolesFile() string { return filepath.Join(c.ConfigDir, "roles.json") }

// SecretFile returns the password file for one credential.
func (c Config) SecretFile(name string) string {
	return filepath.Join(c.SecretsDir, name, "password")
}

// PassFile is the libpq password file the agent writes for replication and
// rewind connections.
func (c Config) PassFile() string { return filepath.Join(c.RunDir, "pgpass") }

// WalG is the path of the wal-g binary.
func (c Config) WalG() string { return filepath.Join(c.BinDir, "wal-g") }

// Getenv is the lookup Load reads from; tests pass a map.
type Getenv func(string) string

// LoadFromEnv reads the process environment.
func LoadFromEnv() (Config, error) { return Load(os.Getenv) }

// Load reads and validates the configuration.
func Load(get Getenv) (Config, error) {
	r := reader{get: get}
	c := Config{
		Topology: cluster.Topology{
			Cluster:   r.required("PGHA_CLUSTER"),
			Namespace: r.required("POD_NAMESPACE"),
			Headless:  r.required("PGHA_HEADLESS_SERVICE"),
			Replicas:  r.int("PGHA_REPLICAS", 3),
		},
		PodName:    r.required("POD_NAME"),
		Revision:   r.str("POD_REVISION", ""),
		DataDir:    r.str("PGDATA", "/var/lib/postgresql/data/pgdata"),
		ConfigDir:  r.str("PGHA_CONFIG_DIR", "/etc/pgha/config"),
		SecretsDir: r.str("PGHA_SECRETS_DIR", "/etc/pgha/secrets"),
		TLSDir:     r.str("PGHA_TLS_DIR", "/etc/pgha/tls"),
		RunDir:     r.str("PGHA_RUN_DIR", "/run/pgha"),
		BinDir:     r.str("PGHA_BIN_DIR", "/pgha/bin"),
		SocketDir:  r.str("PGHA_SOCKET_DIR", "/var/run/postgresql"),
		HTTPAddr:   r.str("PGHA_HTTP_ADDR", ":8008"),
		PeerAddr:   r.str("PGHA_PEER_ADDR", ":8009"),

		LeaseDuration:    r.duration("PGHA_LEASE_DURATION", 15*time.Second),
		RenewDeadline:    r.duration("PGHA_RENEW_DEADLINE", 10*time.Second),
		RetryPeriod:      r.duration("PGHA_RETRY_PERIOD", 2*time.Second),
		RejoinTimeout:    r.duration("PGHA_REJOIN_TIMEOUT", 60*time.Second),
		PromoteTimeout:   r.duration("PGHA_PROMOTE_TIMEOUT", 30*time.Second),
		PeerTimeout:      r.duration("PGHA_PEER_TIMEOUT", 2*time.Second),
		ShutdownTimeout:  r.duration("PGHA_SHUTDOWN_TIMEOUT", 60*time.Second),
		MaxLagOnFailover: r.uint("PGHA_MAX_LAG_ON_FAILOVER", 1<<20),
		SyncMode:         r.str("PGHA_SYNC_MODE", SyncQuorum),
		SyncCount:        r.int("PGHA_SYNC_COUNT", 1),
		ReadyMaxLag:      r.uint("PGHA_READY_MAX_LAG", 64<<20),

		Bootstrap: Bootstrap{
			Mode:       r.str("PGHA_BOOTSTRAP_MODE", BootstrapInitdb),
			InitdbArgs: strings.Fields(r.str("PGHA_INITDB_ARGS", "--encoding=UTF8 --locale=C.UTF-8 --data-checksums")),
			Restore: Restore{
				Backup:          r.str("PGHA_RESTORE_BACKUP", "LATEST"),
				Prefix:          r.str("PGHA_RESTORE_PREFIX", ""),
				TargetTime:      r.str("PGHA_RESTORE_TARGET_TIME", ""),
				TargetLSN:       r.str("PGHA_RESTORE_TARGET_LSN", ""),
				TargetName:      r.str("PGHA_RESTORE_TARGET_NAME", ""),
				TargetExclusive: r.bool("PGHA_RESTORE_TARGET_EXCLUSIVE", false),
			},
		},
		Backup: Backup{
			Enabled:    r.bool("PGHA_BACKUP_ENABLED", false),
			Schedule:   r.str("PGHA_BACKUP_SCHEDULE", "0 3 * * *"),
			Source:     r.str("PGHA_BACKUP_SOURCE", BackupFromStandby),
			RetainFull: r.int("PGHA_BACKUP_RETAIN_FULL", 7),
			RetainDays: r.int("PGHA_BACKUP_RETAIN_DAYS", 14),
			Timeout:    r.duration("PGHA_BACKUP_TIMEOUT", 6*time.Hour),
		},
		PgBouncerService: r.str("PGHA_PGBOUNCER_SERVICE", ""),
		OTLPEndpoint:     r.str("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
	}
	c.Lease = r.str("PGHA_LEASE", c.Topology.Cluster)
	c.BackupLease = r.str("PGHA_BACKUP_LEASE", c.Topology.Cluster+"-backup")

	if err := errors.Join(r.errs...); err != nil {
		return Config{}, err
	}
	return c, c.Validate()
}

// Validate checks the settings fit together.
func (c Config) Validate() error {
	var errs []error
	if err := c.Topology.Validate(); err != nil {
		errs = append(errs, err)
	} else if !c.Topology.IsMember(c.PodName) {
		errs = append(errs, fmt.Errorf("POD_NAME %q is not a member of cluster %q with %d replicas", c.PodName, c.Topology.Cluster, c.Topology.Replicas))
	}
	// The holder must fence itself before any observer can see the Lease as
	// expired, with a retry period of slack for the observers' polling.
	if c.RenewDeadline+c.RetryPeriod >= c.LeaseDuration {
		errs = append(errs, fmt.Errorf("renew deadline (%s) plus retry period (%s) must be less than the lease duration (%s)", c.RenewDeadline, c.RetryPeriod, c.LeaseDuration))
	}
	if c.RetryPeriod <= 0 || c.PeerTimeout <= 0 || c.PromoteTimeout <= 0 {
		errs = append(errs, errors.New("retry period, peer timeout and promote timeout must be positive"))
	}
	if c.PeerTimeout >= c.RetryPeriod*3 {
		errs = append(errs, fmt.Errorf("peer timeout (%s) must be shorter than three retry periods", c.PeerTimeout))
	}
	switch c.SyncMode {
	case SyncQuorum, SyncStrict, SyncOff:
	default:
		errs = append(errs, fmt.Errorf("unknown sync mode %q (want %s, %s or %s)", c.SyncMode, SyncQuorum, SyncStrict, SyncOff))
	}
	if c.SyncCount < 1 {
		errs = append(errs, errors.New("PGHA_SYNC_COUNT must be at least 1"))
	}
	switch c.Bootstrap.Mode {
	case BootstrapInitdb:
	case BootstrapRestore:
		if !c.Backup.Enabled && c.Bootstrap.Restore.Prefix == "" {
			errs = append(errs, errors.New("restore bootstrap needs PGHA_RESTORE_PREFIX or backups enabled"))
		}
		n := 0
		for _, t := range []string{c.Bootstrap.Restore.TargetTime, c.Bootstrap.Restore.TargetLSN, c.Bootstrap.Restore.TargetName} {
			if t != "" {
				n++
			}
		}
		if n > 1 {
			errs = append(errs, errors.New("set at most one restore target (time, LSN or name)"))
		}
	default:
		errs = append(errs, fmt.Errorf("unknown bootstrap mode %q (want %s or %s)", c.Bootstrap.Mode, BootstrapInitdb, BootstrapRestore))
	}
	if c.Backup.Enabled {
		if c.Backup.Source != BackupFromPrimary && c.Backup.Source != BackupFromStandby {
			errs = append(errs, fmt.Errorf("unknown backup source %q (want %s or %s)", c.Backup.Source, BackupFromPrimary, BackupFromStandby))
		}
		if c.Backup.RetainFull < 1 {
			errs = append(errs, errors.New("PGHA_BACKUP_RETAIN_FULL must be at least 1"))
		}
		if c.Backup.RetainDays < 0 {
			errs = append(errs, errors.New("PGHA_BACKUP_RETAIN_DAYS must not be negative"))
		}
	}
	return errors.Join(errs...)
}

type reader struct {
	get  Getenv
	errs []error
}

func (r *reader) str(key, def string) string {
	if v := strings.TrimSpace(r.get(key)); v != "" {
		return v
	}
	return def
}

func (r *reader) required(key string) string {
	v := strings.TrimSpace(r.get(key))
	if v == "" {
		r.errs = append(r.errs, fmt.Errorf("%s is required", key))
	}
	return v
}

func (r *reader) int(key string, def int) int {
	v := r.str(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: %q is not an integer", key, v))
		return def
	}
	return n
}

func (r *reader) uint(key string, def uint64) uint64 {
	v := r.str(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: %q is not a non-negative integer", key, v))
		return def
	}
	return n
}

func (r *reader) bool(key string, def bool) bool {
	v := r.str(key, "")
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: %q is not a boolean", key, v))
		return def
	}
	return b
}

func (r *reader) duration(key string, def time.Duration) time.Duration {
	v := r.str(key, "")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: %q is not a duration", key, v))
		return def
	}
	return d
}
