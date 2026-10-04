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

import "testing"

func topo() Topology {
	return Topology{Cluster: "pg", Namespace: "db", Headless: "pg-headless", Replicas: 3}
}

func TestMembersAndOrdinals(t *testing.T) {
	tp := topo()
	got := tp.Members()
	want := []string{"pg-0", "pg-1", "pg-2"}
	if len(got) != len(want) {
		t.Fatalf("members = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("members = %v, want %v", got, want)
		}
	}
	for _, tc := range []struct {
		pod string
		n   int
		ok  bool
	}{
		{"pg-0", 0, true},
		{"pg-2", 2, true},
		{"pg-3", 0, false},
		{"pg-01", 0, false},
		{"pg-", 0, false},
		{"other-0", 0, false},
		{"pg--1", 0, false},
	} {
		n, ok := tp.Ordinal(tc.pod)
		if n != tc.n || ok != tc.ok {
			t.Errorf("Ordinal(%q) = %d,%v want %d,%v", tc.pod, n, ok, tc.n, tc.ok)
		}
	}
}

func TestHostAndSAN(t *testing.T) {
	tp := topo()
	if h := tp.Host("pg-1"); h != "pg-1.pg-headless.db.svc" {
		t.Fatalf("Host = %q", h)
	}
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"*.pg-headless.db.svc", true},
		{"pg-1.pg-headless.db.svc", true},
		{"PG-1.pg-headless.db.svc.", true},
		{"pg-1.pg-headless.other.svc", false},
		{"pg-1.other-headless.db.svc", false},
		{"pg-7.pg-headless.db.svc", false},
		{"pg-1", false},
		{"*.pg-headless.other.svc", false},
	} {
		if got := tp.IsMemberSAN(tc.name); got != tc.ok {
			t.Errorf("IsMemberSAN(%q) = %v, want %v", tc.name, got, tc.ok)
		}
	}
}

func TestSlotName(t *testing.T) {
	if got := SlotName("My-Cluster-2"); got != "my_cluster_2" {
		t.Fatalf("SlotName = %q", got)
	}
	if got := topo().SlotPrefix(); got != "pg_" {
		t.Fatalf("SlotPrefix = %q", got)
	}
}

func TestValidate(t *testing.T) {
	if err := topo().Validate(); err != nil {
		t.Fatal(err)
	}
	bad := topo()
	bad.Replicas = 0
	if bad.Validate() == nil {
		t.Fatal("zero replicas accepted")
	}
}
