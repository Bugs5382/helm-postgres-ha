package pooler

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
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	golog "github.com/Bugs5382/go-log"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type fakeAdmin struct {
	mu   sync.Mutex
	dbs  map[string][]string
	down map[string]bool
	ran  []string
}

func (f *fakeAdmin) Databases(_ context.Context, addr string) ([]string, error) {
	if f.down[addr] {
		return nil, errors.New("connect timeout")
	}
	return f.dbs[addr], nil
}

func (f *fakeAdmin) Exec(_ context.Context, addr, cmd string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ran = append(f.ran, addr+" "+cmd)
	return nil
}

type staticEndpoints []string

func (s staticEndpoints) Addresses(context.Context) ([]string, error) { return s, nil }

func TestResetKillsAndResumesEveryDatabaseOnEveryPooler(t *testing.T) {
	a := &fakeAdmin{dbs: map[string][]string{
		"10.0.0.1": {"pgbouncer", "app", "postgres"},
		"10.0.0.2": {"pgbouncer", "app"},
	}}
	n, err := New(staticEndpoints{"10.0.0.1", "10.0.0.2"}, a, golog.Nop()).Reset(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("reset %d poolers, err %v", n, err)
	}
	sort.Strings(a.ran)
	want := []string{
		"10.0.0.1 KILL app", "10.0.0.1 KILL postgres", "10.0.0.1 RESUME app", "10.0.0.1 RESUME postgres",
		"10.0.0.2 KILL app", "10.0.0.2 RESUME app",
	}
	if !slices.Equal(a.ran, want) {
		t.Fatalf("ran %q, want %q", a.ran, want)
	}
}

func TestKillComesBeforeResumeOnEachDatabase(t *testing.T) {
	a := &fakeAdmin{dbs: map[string][]string{"10.0.0.1": {"app"}}}
	if _, err := New(staticEndpoints{"10.0.0.1"}, a, golog.Nop()).Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(a.ran, []string{"10.0.0.1 KILL app", "10.0.0.1 RESUME app"}) {
		t.Fatalf("ran %q", a.ran)
	}
}

func TestResetSkipsDeadPoolersAndOddNames(t *testing.T) {
	a := &fakeAdmin{
		dbs:  map[string][]string{"10.0.0.1": {"app", `bad"name`, "x;y"}},
		down: map[string]bool{"10.0.0.2": true},
	}
	n, err := New(staticEndpoints{"10.0.0.1", "10.0.0.2"}, a, golog.Nop()).Reset(context.Background())
	if n != 1 || err == nil || !strings.Contains(err.Error(), "10.0.0.2") {
		t.Fatalf("reset %d, err %v", n, err)
	}
	for _, r := range a.ran {
		if strings.Contains(r, "bad") || strings.Contains(r, ";") {
			t.Fatalf("ran an unsafe name: %q", r)
		}
	}
}

func TestEndpointSliceAddresses(t *testing.T) {
	ready, notReady := true, false
	cs := fake.NewClientset(
		&discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{Name: "pg-pgbouncer-abc", Namespace: "db", Labels: map[string]string{discoveryv1.LabelServiceName: "pg-pgbouncer"}},
			Endpoints: []discoveryv1.Endpoint{
				{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}},
				{Addresses: []string{"10.0.0.2"}, Conditions: discoveryv1.EndpointConditions{Ready: &notReady}},
			},
		},
		&discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "db", Labels: map[string]string{discoveryv1.LabelServiceName: "other"}},
			Endpoints:  []discoveryv1.Endpoint{{Addresses: []string{"10.9.9.9"}}},
		},
	)
	got, err := NewEndpoints(cs, "db", "pg-pgbouncer").Addresses(context.Background())
	sort.Strings(got)
	if err != nil || !slices.Equal(got, []string{"10.0.0.1", "10.0.0.2"}) {
		t.Fatalf("addresses = %v, %v", got, err)
	}
}
