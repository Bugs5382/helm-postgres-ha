// Package election holds the pure decisions a member makes when the Lease has
// no live holder: whether a cluster with no data may be bootstrapped by this
// member, which standby should be promoted, and who a departing primary hands
// over to. It has no I/O, so every rule is tested directly.
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
	"fmt"
	"sort"

	"github.com/Bugs5382/helm-postgres-ha/internal/cluster"
	"github.com/Bugs5382/helm-postgres-ha/internal/peer"
)

// Input is everything a decision looks at.
type Input struct {
	Self     string
	Topology cluster.Topology
	// Members holds every member's answer, this member's own included.
	Members map[string]peer.Result
	// SystemID is the cluster's identifier from the Lease; empty before the
	// first bootstrap.
	SystemID string
	// LastTimeline and LastLSN are the position the last primary recorded on
	// the Lease.
	LastTimeline int
	LastLSN      uint64
	// MaxLag is how far behind LastLSN a candidate may be.
	MaxLag uint64
	// Successor is the member a departing primary handed over to, honoured
	// while SuccessorFresh is true.
	Successor      string
	SuccessorFresh bool
	// OldRevision is the last primary's controller revision; members on a
	// different (newer) revision are preferred, so a rolling update does not
	// promote a pod that is about to be replaced.
	OldRevision string
}

// Decision is the outcome.
type Decision struct {
	Acquire bool
	// Best is the member that should act, when there is one.
	Best   string
	Reason string
}

// Quorum is how many members must answer before a decision is made: a strict
// majority, except that a two-member cluster cannot have one and accepts a
// single answer (the Lease itself is the arbiter there).
func Quorum(replicas int) int {
	if replicas == 2 {
		return 1
	}
	return replicas/2 + 1
}

func reachable(in Input) []string {
	var out []string
	for _, m := range in.Topology.Members() {
		if r, ok := in.Members[m]; ok && r.Err == nil {
			out = append(out, m)
		}
	}
	return out
}

// Bootstrap decides whether this member, which has no data, may create the
// cluster. Only the lowest-ordinal reachable member does, and only when a
// quorum answered and none of them has data.
func Bootstrap(in Input) Decision {
	up := reachable(in)
	if q := Quorum(in.Topology.Replicas); len(up) < q {
		return Decision{Reason: fmt.Sprintf("only %d of %d members answered, need %d to bootstrap", len(up), in.Topology.Replicas, q)}
	}
	for _, m := range up {
		if in.Members[m].Status.HasData {
			return Decision{Best: m, Reason: fmt.Sprintf("member %s already has data", m)}
		}
	}
	// up is in ordinal order.
	if up[0] != in.Self {
		return Decision{Best: up[0], Reason: fmt.Sprintf("member %s bootstraps the cluster", up[0])}
	}
	return Decision{Acquire: true, Best: in.Self, Reason: "no member has data; bootstrapping"}
}

type candidate struct {
	name    string
	ordinal int
	s       peer.Status
}

func rank(cands []candidate, oldRevision string) {
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.s.Timeline != b.s.Timeline {
			return a.s.Timeline > b.s.Timeline
		}
		if a.s.Position != b.s.Position {
			return a.s.Position > b.s.Position
		}
		if oldRevision != "" {
			an, bn := a.s.Revision != oldRevision, b.s.Revision != oldRevision
			if an != bn {
				return an
			}
		}
		return a.ordinal < b.ordinal
	})
}

// Failover decides whether this member should take the expired Lease and be
// promoted. It refuses unless a quorum answered, no member still runs
// read-write or still streams from a primary, and this member is the most
// complete eligible standby within the lag limit.
func Failover(in Input) Decision {
	up := reachable(in)
	if q := Quorum(in.Topology.Replicas); len(up) < q {
		return Decision{Reason: fmt.Sprintf("only %d of %d members answered, need %d to fail over", len(up), in.Topology.Replicas, q)}
	}
	var cands []candidate
	for _, m := range up {
		s := in.Members[m].Status
		if s.Running && !s.InRecovery {
			return Decision{Reason: fmt.Sprintf("member %s still runs read-write", m)}
		}
		if s.Streaming {
			return Decision{Reason: fmt.Sprintf("member %s still streams from %s", m, s.Upstream)}
		}
		if !s.Eligible || !s.HasData {
			continue
		}
		if in.SystemID != "" && s.SystemID != in.SystemID {
			continue
		}
		n, _ := in.Topology.Ordinal(m)
		cands = append(cands, candidate{name: m, ordinal: n, s: s})
	}
	if len(cands) == 0 {
		return Decision{Reason: "no eligible standby answered"}
	}
	rank(cands, in.OldRevision)
	best := cands[0]
	if in.SuccessorFresh {
		for _, c := range cands {
			if c.name == in.Successor {
				best = c
				break
			}
		}
	}
	if best.name != in.Self {
		return Decision{Best: best.name, Reason: fmt.Sprintf("member %s is the better candidate", best.name)}
	}
	if in.LastLSN > 0 && best.s.Timeline <= in.LastTimeline && in.LastLSN > best.s.Position && in.LastLSN-best.s.Position > in.MaxLag {
		return Decision{Best: best.name, Reason: fmt.Sprintf("%d bytes behind the last primary position, over the %d byte limit", in.LastLSN-best.s.Position, in.MaxLag)}
	}
	return Decision{Acquire: true, Best: in.Self, Reason: "most complete eligible standby"}
}

// Successor picks the standby a departing primary hands over to: a member
// that is streaming and eligible, preferring one on a different revision from
// the primary, then the most WAL, then the lowest ordinal. It returns "" when
// none qualifies.
func Successor(topo cluster.Topology, self, revision string, members map[string]peer.Result) string {
	var cands []candidate
	for _, m := range topo.Members() {
		r, ok := members[m]
		if m == self || !ok || r.Err != nil || !r.Status.Streaming || !r.Status.Eligible {
			continue
		}
		n, _ := topo.Ordinal(m)
		cands = append(cands, candidate{name: m, ordinal: n, s: r.Status})
	}
	if len(cands) == 0 {
		return ""
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if revision != "" {
			an, bn := a.s.Revision != revision, b.s.Revision != revision
			if an != bn {
				return an
			}
		}
		if a.s.Position != b.s.Position {
			return a.s.Position > b.s.Position
		}
		return a.ordinal < b.ordinal
	})
	return cands[0].name
}
