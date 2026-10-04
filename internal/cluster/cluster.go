// Package cluster derives the names of a cluster's members from its topology:
// pod names, their stable DNS names, replication slot names and the
// certificate names peers present to each other.
package cluster

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
	"strconv"
	"strings"
)

// Topology is the static shape of one cluster, as rendered by the chart.
type Topology struct {
	// Cluster is the StatefulSet name; members are <Cluster>-0..<Replicas-1>.
	Cluster string
	// Namespace the cluster runs in.
	Namespace string
	// Headless is the governing headless Service of the StatefulSet.
	Headless string
	// Replicas is the number of members.
	Replicas int
}

// PodName returns the name of the member with the given ordinal.
func (t Topology) PodName(ordinal int) string {
	return t.Cluster + "-" + strconv.Itoa(ordinal)
}

// Members returns every member's pod name in ordinal order.
func (t Topology) Members() []string {
	out := make([]string, 0, t.Replicas)
	for i := 0; i < t.Replicas; i++ {
		out = append(out, t.PodName(i))
	}
	return out
}

// Ordinal returns the ordinal of a member's pod name, and false when the name
// is not a member of this cluster.
func (t Topology) Ordinal(pod string) (int, bool) {
	rest, ok := strings.CutPrefix(pod, t.Cluster+"-")
	if !ok || rest == "" {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 0 || n >= t.Replicas || strconv.Itoa(n) != rest {
		return 0, false
	}
	return n, true
}

// IsMember reports whether pod is one of this cluster's members.
func (t Topology) IsMember(pod string) bool {
	_, ok := t.Ordinal(pod)
	return ok
}

// domain is the per-pod DNS suffix the headless Service publishes.
func (t Topology) domain() string {
	return t.Headless + "." + t.Namespace + ".svc"
}

// Host returns the stable DNS name of a member: <pod>.<headless>.<namespace>.svc.
func (t Topology) Host(pod string) string {
	return pod + "." + t.domain()
}

// WildcardSAN is the DNS name the shared cluster certificate carries for its
// members.
func (t Topology) WildcardSAN() string {
	return "*." + t.domain()
}

// IsMemberSAN reports whether a certificate DNS name identifies a member of
// this cluster: the cluster wildcard, or one member's full host name. The match
// is on the whole name, so a certificate for the same pod name in another
// namespace or behind another Service does not pass.
func (t Topology) IsMemberSAN(name string) bool {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	if name == t.WildcardSAN() {
		return true
	}
	pod, ok := strings.CutSuffix(name, "."+t.domain())
	return ok && t.IsMember(pod)
}

// SlotName returns the physical replication slot name for a member. Slot names
// allow only lower-case letters, digits and underscores.
func SlotName(pod string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(pod) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// SlotPrefix is the prefix every slot this cluster manages starts with, so the
// agent never drops a slot someone else created.
func (t Topology) SlotPrefix() string {
	return SlotName(t.Cluster + "-")
}

// Validate checks the topology is usable.
func (t Topology) Validate() error {
	switch {
	case t.Cluster == "":
		return fmt.Errorf("cluster name is empty")
	case t.Namespace == "":
		return fmt.Errorf("namespace is empty")
	case t.Headless == "":
		return fmt.Errorf("headless service name is empty")
	case t.Replicas < 1:
		return fmt.Errorf("replicas must be at least 1, got %d", t.Replicas)
	}
	return nil
}
