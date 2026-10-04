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
	"path/filepath"
	"strconv"
	"time"

	golog "github.com/Bugs5382/go-log"

	"github.com/Bugs5382/helm-postgres-ha/internal/lease"
	"github.com/Bugs5382/helm-postgres-ha/internal/peer"
)

// selfStatus is what this member tells its peers.
func (a *Agent) selfStatus(l local, rec lease.Record) peer.Status {
	s := peer.Status{
		Pod:        a.me,
		Role:       peer.RoleStopped,
		Running:    l.running,
		HasData:    l.hasData,
		SystemID:   l.systemID,
		InRecovery: l.st.InRecovery,
		Timeline:   l.st.Timeline,
		Position:   uint64(l.st.Position()),
		ReplayLSN:  uint64(l.st.ReplayLSN),
		Streaming:  l.st.Streaming(),
		Upstream:   l.st.SenderHost,
		Revision:   a.cfg.Revision,
	}
	if a.task != nil {
		s.Busy = a.task.name
		if a.task.name == "initdb" || a.task.name == "restore" {
			s.Role = peer.RoleBootstrapping
		}
	}
	switch {
	case l.running && l.up && !l.st.InRecovery:
		s.Role = peer.RolePrimary
	case l.running && l.up:
		s.Role = peer.RoleStandby
	}
	s.Eligible = l.running && l.up && l.st.InRecovery && !l.recovery && a.fatal == nil && a.task == nil && a.systemIDMismatch(rec, l) == nil
	return s
}

// publish stores the status and readiness and updates the gauges.
func (a *Agent) publish(l local, rec lease.Record) {
	s := a.selfStatus(l, rec)
	a.status.Store(&s)
	a.m.SetRole(s.Role)
	a.m.Timeline.Set(float64(s.Timeline))
	a.m.WALPosition.Set(float64(s.Position))
	if s.Streaming {
		a.m.Streaming.Set(1)
	} else {
		a.m.Streaming.Set(0)
	}
	ready := false
	switch s.Role {
	case peer.RolePrimary:
		ready = a.holdingRW.Load() && rec.Holder == a.me
	case peer.RoleStandby:
		ready = s.Streaming && a.lagOK(l, rec)
	}
	if ready != a.ready.Load() {
		a.log.Info("readiness changed", golog.F("ready", ready), golog.F("role", s.Role))
	}
	a.ready.Store(ready)
	if ready {
		a.m.Ready.Set(1)
	} else {
		a.m.Ready.Set(0)
	}
	a.collectDisk()
}

// lagOK compares a standby's replay position with the one the primary last
// recorded on the Lease.
func (a *Agent) lagOK(l local, rec lease.Record) bool {
	last, err := strconv.ParseUint(rec.Ann(lease.LSN), 10, 64)
	tl, _ := strconv.Atoi(rec.Ann(lease.Timeline))
	if err != nil || tl != l.st.Timeline {
		a.m.ReplayLagBytes.Set(0)
		return true
	}
	lag := uint64(0)
	if replay := uint64(l.st.ReplayLSN); last > replay {
		lag = last - replay
	}
	a.m.ReplayLagBytes.Set(float64(lag))
	return a.cfg.ReadyMaxLag == 0 || lag <= a.cfg.ReadyMaxLag
}

func (a *Agent) collectDisk() {
	if a.now().Sub(a.lastDisk) < dutyEvery {
		return
	}
	a.lastDisk = a.now()
	size, avail, wal, err := a.node.Disk()
	if err != nil {
		golog.Trace(a.log, "cannot read disk usage", golog.F("error", err.Error()))
		return
	}
	a.m.DataVolumeSize.Set(float64(size))
	a.m.DataVolumeAvailable.Set(float64(avail))
	a.m.WALDirBytes.Set(float64(wal))
}

// refreshPassFile rewrites the libpq password file when a password changes.
func (a *Agent) refreshPassFile() {
	stamp := a.stamp(a.cfg.SecretFile("replication")) + a.stamp(a.cfg.SecretFile("rewind"))
	if stamp == a.passStamp {
		return
	}
	entries, err := a.passEntries()
	if err != nil {
		a.log.Error(err, "cannot read replication credentials")
		return
	}
	if err := a.node.WritePassFile(entries); err != nil {
		a.log.Error(err, "cannot write the password file")
		return
	}
	a.passStamp = stamp
	a.log.Debug("password file written")
}

// reloadOnChange signals PostgreSQL when the mounted configuration or
// certificates change, so a values change reaches a running cluster.
func (a *Agent) reloadOnChange(l local) {
	stamp := a.stamp(a.cfg.ConfigFile()) + a.stamp(a.cfg.HBAFile())
	for _, f := range []string{"tls.crt", "tls.key", "ca.crt"} {
		stamp += a.stamp(filepath.Join(a.cfg.TLSDir, f))
	}
	if stamp == a.fileStamps {
		return
	}
	first := a.fileStamps == ""
	a.fileStamps = stamp
	if first || !l.running {
		return
	}
	if err := a.node.Reload(); err != nil {
		a.log.Warn("cannot reload postgres after a configuration change", golog.F("error", err.Error()))
		return
	}
	a.log.Info("configuration or certificates changed; postgres reloaded")
	go a.reportPendingRestart()
}

// reportPendingRestart logs settings that only take effect after a restart.
// The chart rolls the pods for those, so this is a cross-check.
func (a *Agent) reportPendingRestart() {
	time.Sleep(2 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.PeerTimeout)
	defer cancel()
	names, err := a.node.PendingRestart(ctx)
	if err != nil {
		return
	}
	a.m.PendingRestart.Set(float64(len(names)))
	if len(names) > 0 {
		a.log.Warn("settings changed that need a restart", golog.F("settings", names))
	}
}
