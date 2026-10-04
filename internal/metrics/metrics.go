// Package metrics is the agent's Prometheus surface, scraped from the
// /metrics route of the plain HTTP listener.
package metrics

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
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const ns = "pgha"

// Set holds every metric the agent exports.
type Set struct {
	reg *prometheus.Registry

	Role            *prometheus.GaugeVec
	LeaseHolder     prometheus.Gauge
	Timeline        prometheus.Gauge
	WALPosition     prometheus.Gauge
	ReplayLagBytes  prometheus.Gauge
	Streaming       prometheus.Gauge
	Ready           prometheus.Gauge
	Promotions      prometheus.Counter
	PromoteFailures prometheus.Counter
	Fencings        prometheus.Counter
	Rewinds         *prometheus.CounterVec
	Clones          *prometheus.CounterVec
	SplitBrain      prometheus.Gauge
	PeersReachable  prometheus.Gauge
	PendingRestart  prometheus.Gauge
	TickSeconds     prometheus.Histogram
	LastTick        prometheus.Gauge
	CodedErrors     *prometheus.CounterVec

	ArchiveFailed       prometheus.Gauge
	ArchiveArchived     prometheus.Gauge
	ArchiveLastSuccess  prometheus.Gauge
	ArchiveLastFailure  prometheus.Gauge
	BackupLastSuccess   prometheus.Gauge
	BackupFailures      prometheus.Counter
	BackupDuration      prometheus.Gauge
	BackupLastVerify    prometheus.Gauge
	BackupVerifyOK      prometheus.Gauge
	DataVolumeSize      prometheus.Gauge
	DataVolumeAvailable prometheus.Gauge
	WALDirBytes         prometheus.Gauge
	ReplicaLagBytes     *prometheus.GaugeVec
}

// New registers the metrics.
func New() *Set {
	reg := prometheus.NewRegistry()
	f := func(c prometheus.Collector) { reg.MustRegister(c) }
	g := func(name, help string) prometheus.Gauge {
		m := prometheus.NewGauge(prometheus.GaugeOpts{Namespace: ns, Name: name, Help: help})
		f(m)
		return m
	}
	c := func(name, help string) prometheus.Counter {
		m := prometheus.NewCounter(prometheus.CounterOpts{Namespace: ns, Name: name, Help: help})
		f(m)
		return m
	}
	s := &Set{reg: reg}
	f(collectors.NewGoCollector())
	f(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	s.Role = prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: ns, Name: "role", Help: "1 for the role this member runs as (primary, standby, stopped, bootstrapping)."}, []string{"role"})
	f(s.Role)
	s.LeaseHolder = g("lease_holder", "1 while this member holds the cluster Lease.")
	s.Timeline = g("timeline", "The member's current timeline.")
	s.WALPosition = g("wal_position_bytes", "The member's WAL position: write position on a primary, received position on a standby.")
	s.ReplayLagBytes = g("replay_lag_bytes", "How far a standby's replay is behind the position the primary last recorded.")
	s.Streaming = g("streaming", "1 while a standby's WAL receiver is streaming.")
	s.Ready = g("ready", "1 while the member reports ready for its role.")
	s.Promotions = c("promotions_total", "Promotions of this member to primary.")
	s.PromoteFailures = c("promote_failures_total", "Promotions that did not finish in time.")
	s.Fencings = c("fencings_total", "Times this member stopped PostgreSQL because it was read-write without the Lease.")
	s.Rewinds = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "rewinds_total", Help: "pg_rewind runs by outcome."}, []string{"outcome"})
	f(s.Rewinds)
	s.Clones = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "clones_total", Help: "Clones from the primary by outcome."}, []string{"outcome"})
	f(s.Clones)
	s.SplitBrain = g("split_brain", "1 while another member reports running read-write alongside this primary.")
	s.PeersReachable = g("peers_reachable", "Members (this one included) that answered the last status round.")
	s.PendingRestart = g("pending_restart_settings", "Settings changed in the configuration that need a restart.")
	s.TickSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: ns, Name: "tick_seconds", Help: "Duration of one reconcile round.", Buckets: prometheus.ExponentialBuckets(0.005, 2, 12)})
	f(s.TickSeconds)
	s.LastTick = g("last_tick_timestamp_seconds", "When the last reconcile round finished.")
	s.CodedErrors = prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "errors_total", Help: "Coded errors by code (see docs/errors.md)."}, []string{"code"})
	f(s.CodedErrors)
	s.ArchiveFailed = g("archive_failed_count", "pg_stat_archiver.failed_count on the primary.")
	s.ArchiveArchived = g("archive_archived_count", "pg_stat_archiver.archived_count on the primary.")
	s.ArchiveLastSuccess = g("archive_last_success_timestamp_seconds", "When the primary last archived a WAL segment.")
	s.ArchiveLastFailure = g("archive_last_failure_timestamp_seconds", "When archiving last failed on the primary.")
	s.BackupLastSuccess = g("backup_last_success_timestamp_seconds", "When the cluster's last base backup finished, as recorded on the backup Lease.")
	s.BackupFailures = c("backup_failures_total", "Base backups run by this member that failed.")
	s.BackupDuration = g("backup_last_duration_seconds", "Duration of the last base backup this member ran.")
	s.BackupLastVerify = g("backup_last_verify_timestamp_seconds", "When the scheduled restore check last ran, as recorded on the backup Lease.")
	s.BackupVerifyOK = g("backup_last_verify_ok", "1 when the last scheduled restore check restored the latest backup and answered its query.")
	s.DataVolumeSize = g("data_volume_size_bytes", "Size of the data volume.")
	s.DataVolumeAvailable = g("data_volume_available_bytes", "Free space on the data volume.")
	s.WALDirBytes = g("wal_dir_bytes", "Size of pg_wal.")
	s.ReplicaLagBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: ns, Name: "replica_replay_lag_bytes", Help: "Replay lag of each attached standby, seen from the primary."}, []string{"member", "sync_state"})
	f(s.ReplicaLagBytes)
	return s
}

// SetRole sets the role gauge to one role.
func (s *Set) SetRole(role string) {
	for _, r := range []string{"primary", "standby", "stopped", "bootstrapping"} {
		v := 0.0
		if r == role {
			v = 1
		}
		s.Role.WithLabelValues(r).Set(v)
	}
}

// Handler serves the metrics.
func (s *Set) Handler() http.Handler {
	return promhttp.HandlerFor(s.reg, promhttp.HandlerOpts{})
}

// Gather is for tests.
func (s *Set) Gather() (map[string]float64, error) {
	mfs, err := s.reg.Gather()
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			name := mf.GetName()
			for _, l := range m.GetLabel() {
				name += "," + l.GetName() + "=" + l.GetValue()
			}
			switch {
			case m.Gauge != nil:
				out[name] = m.GetGauge().GetValue()
			case m.Counter != nil:
				out[name] = m.GetCounter().GetValue()
			}
		}
	}
	return out, nil
}
