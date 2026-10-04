package election

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
	"strings"
	"testing"

	"github.com/Bugs5382/helm-postgres-ha/internal/cluster"
	"github.com/Bugs5382/helm-postgres-ha/internal/peer"
)

var topo = cluster.Topology{Cluster: "pg", Namespace: "db", Headless: "pg-headless", Replicas: 3}

var down = peer.Result{Err: errors.New("unreachable")}

func standby(pos uint64, tl int) peer.Result {
	return peer.Result{Status: peer.Status{Role: peer.RoleStandby, Running: true, HasData: true, InRecovery: true, Eligible: true, Timeline: tl, Position: pos, SystemID: "7"}}
}

func input(self string, m map[string]peer.Result) Input {
	return Input{Self: self, Topology: topo, Members: m, SystemID: "7", MaxLag: 1 << 20}
}

func TestQuorum(t *testing.T) {
	for n, want := range map[int]int{1: 1, 2: 1, 3: 2, 4: 3, 5: 3} {
		if got := Quorum(n); got != want {
			t.Errorf("Quorum(%d) = %d, want %d", n, got, want)
		}
	}
}

func TestFailoverPicksMostReceivedWAL(t *testing.T) {
	m := map[string]peer.Result{"pg-0": down, "pg-1": standby(100, 1), "pg-2": standby(200, 1)}
	if d := Failover(input("pg-1", m)); d.Acquire || d.Best != "pg-2" {
		t.Fatalf("pg-1 decided %+v", d)
	}
	if d := Failover(input("pg-2", m)); !d.Acquire {
		t.Fatalf("pg-2 decided %+v", d)
	}
}

func TestFailoverRanksReceiveOverReplay(t *testing.T) {
	// Position is the received LSN; pg-1 has received more but replayed less.
	a, b := standby(300, 1), standby(250, 1)
	a.Status.ReplayLSN, b.Status.ReplayLSN = 100, 250
	m := map[string]peer.Result{"pg-0": down, "pg-1": a, "pg-2": b}
	if d := Failover(input("pg-1", m)); !d.Acquire {
		t.Fatalf("the member with more received WAL was not chosen: %+v", d)
	}
}

func TestFailoverPrefersHigherTimeline(t *testing.T) {
	m := map[string]peer.Result{"pg-0": down, "pg-1": standby(900, 1), "pg-2": standby(100, 2)}
	if d := Failover(input("pg-2", m)); !d.Acquire {
		t.Fatalf("higher timeline not preferred: %+v", d)
	}
}

func TestFailoverNeedsQuorum(t *testing.T) {
	m := map[string]peer.Result{"pg-0": down, "pg-1": standby(100, 1), "pg-2": down}
	d := Failover(input("pg-1", m))
	if d.Acquire || !strings.Contains(d.Reason, "need 2") {
		t.Fatalf("decided without quorum: %+v", d)
	}
}

func TestFailoverRefusesWhileAPrimaryIsSeen(t *testing.T) {
	rw := peer.Result{Status: peer.Status{Role: peer.RolePrimary, Running: true, HasData: true}}
	streaming := standby(100, 1)
	streaming.Status.Streaming = true
	streaming.Status.Upstream = "pg-0.pg-headless.db.svc"
	for name, m := range map[string]map[string]peer.Result{
		"read-write member": {"pg-0": rw, "pg-1": standby(100, 1), "pg-2": standby(50, 1)},
		"peer streaming":    {"pg-0": down, "pg-1": standby(100, 1), "pg-2": streaming},
	} {
		if d := Failover(input("pg-1", m)); d.Acquire {
			t.Errorf("%s: promoted anyway (%+v)", name, d)
		}
	}
	self := standby(100, 1)
	self.Status.Streaming = true
	if d := Failover(input("pg-1", map[string]peer.Result{"pg-0": down, "pg-1": self, "pg-2": standby(50, 1)})); d.Acquire {
		t.Fatal("promoted while its own receiver still streams")
	}
}

func TestFailoverSkipsForeignAndIneligible(t *testing.T) {
	foreign := standby(999, 1)
	foreign.Status.SystemID = "8"
	busy := standby(999, 1)
	busy.Status.Eligible = false
	m := map[string]peer.Result{"pg-0": foreign, "pg-1": standby(100, 1), "pg-2": busy}
	if d := Failover(input("pg-1", m)); !d.Acquire {
		t.Fatalf("foreign or ineligible member blocked a valid candidate: %+v", d)
	}
}

func TestFailoverLagLimit(t *testing.T) {
	m := map[string]peer.Result{"pg-0": down, "pg-1": standby(100, 1), "pg-2": standby(50, 1)}
	in := input("pg-1", m)
	in.LastTimeline, in.LastLSN, in.MaxLag = 1, 100+(2<<20), 1<<20
	if d := Failover(in); d.Acquire || !strings.Contains(d.Reason, "over the") {
		t.Fatalf("promoted beyond the lag limit: %+v", d)
	}
	in.LastLSN = 100 + 512
	if d := Failover(in); !d.Acquire {
		t.Fatalf("refused within the lag limit: %+v", d)
	}
	// A candidate already on a newer timeline is not compared against an
	// older timeline's position.
	m["pg-1"] = standby(10, 2)
	in.LastLSN = 1 << 40
	if d := Failover(in); !d.Acquire {
		t.Fatalf("newer timeline compared against old LSN: %+v", d)
	}
}

func TestFailoverHonoursSuccessorAndRevision(t *testing.T) {
	m := map[string]peer.Result{"pg-0": down, "pg-1": standby(100, 1), "pg-2": standby(100, 1)}
	in := input("pg-2", m)
	in.Successor, in.SuccessorFresh = "pg-2", true
	if d := Failover(in); !d.Acquire {
		t.Fatalf("named successor not honoured: %+v", d)
	}
	in.SuccessorFresh = false
	if d := Failover(in); d.Acquire {
		t.Fatalf("stale successor still honoured: %+v", d)
	}
	a, b := standby(100, 1), standby(100, 1)
	a.Status.Revision, b.Status.Revision = "old", "new"
	in = input("pg-2", map[string]peer.Result{"pg-0": down, "pg-1": a, "pg-2": b})
	in.OldRevision = "old"
	if d := Failover(in); !d.Acquire {
		t.Fatalf("updated member not preferred: %+v", d)
	}
}

func TestBootstrap(t *testing.T) {
	empty := peer.Result{Status: peer.Status{Role: peer.RoleStopped}}
	m := map[string]peer.Result{"pg-0": empty, "pg-1": empty, "pg-2": empty}
	if d := Bootstrap(input("pg-0", m)); !d.Acquire {
		t.Fatalf("pg-0 refused: %+v", d)
	}
	if d := Bootstrap(input("pg-1", m)); d.Acquire || d.Best != "pg-0" {
		t.Fatalf("pg-1 decided %+v", d)
	}
	// Pod 0 is gone: the next lowest reachable member bootstraps instead of
	// waiting on an ordinal.
	m["pg-0"] = down
	if d := Bootstrap(input("pg-1", m)); !d.Acquire {
		t.Fatalf("pg-1 refused with pg-0 down: %+v", d)
	}
	m["pg-2"] = standby(1, 1)
	if d := Bootstrap(input("pg-1", m)); d.Acquire {
		t.Fatalf("bootstrapped while a member has data: %+v", d)
	}
	if d := Bootstrap(input("pg-0", map[string]peer.Result{"pg-0": empty, "pg-1": down, "pg-2": down})); d.Acquire {
		t.Fatalf("bootstrapped without quorum: %+v", d)
	}
}

func TestSuccessor(t *testing.T) {
	s := func(pos uint64, rev string, streaming bool) peer.Result {
		r := standby(pos, 1)
		r.Status.Streaming, r.Status.Revision = streaming, rev
		return r
	}
	m := map[string]peer.Result{"pg-0": s(0, "old", false), "pg-1": s(100, "old", true), "pg-2": s(90, "new", true)}
	if got := Successor(topo, "pg-0", "old", m); got != "pg-2" {
		t.Fatalf("successor = %q, want the updated member", got)
	}
	if got := Successor(topo, "pg-0", "", m); got != "pg-1" {
		t.Fatalf("successor = %q, want the most WAL", got)
	}
	m["pg-1"], m["pg-2"] = s(1, "x", false), down
	if got := Successor(topo, "pg-0", "", m); got != "" {
		t.Fatalf("successor = %q, want none", got)
	}
}
