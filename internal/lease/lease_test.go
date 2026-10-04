package lease

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
	"testing"
	"time"

	coordv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) add(d time.Duration)     { c.t = c.t.Add(d) }
func newClock() *clock                   { return &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }
func lease(holder string) *coordv1.Lease { return withHolder(holder, "1") }

func withHolder(holder, rv string) *coordv1.Lease {
	l := &coordv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: "db", ResourceVersion: rv}}
	if holder != "" {
		l.Spec.HolderIdentity = &holder
		now := metav1.NewMicroTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		l.Spec.RenewTime = &now
	}
	return l
}

// enforceResourceVersion makes the fake API reject stale updates the way the
// real API server does, so the election's optimistic concurrency is tested.
func enforceResourceVersion(cs *fake.Clientset) {
	cs.PrependReactor("update", "leases", func(a k8stesting.Action) (bool, runtime.Object, error) {
		upd := a.(k8stesting.UpdateAction).GetObject().(*coordv1.Lease)
		cur, err := cs.Tracker().Get(schema.GroupVersionResource{Group: "coordination.k8s.io", Version: "v1", Resource: "leases"}, upd.Namespace, upd.Name)
		if err != nil {
			return true, nil, err
		}
		if cur.(*coordv1.Lease).ResourceVersion != upd.ResourceVersion {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, upd.Name, errors.New("stale"))
		}
		next := upd.DeepCopy()
		next.ResourceVersion = upd.ResourceVersion + "1"
		if err := cs.Tracker().Update(schema.GroupVersionResource{Group: "coordination.k8s.io", Version: "v1", Resource: "leases"}, next, upd.Namespace); err != nil {
			return true, nil, err
		}
		return true, next, nil
	})
}

func TestExpiryIsJudgedByObservation(t *testing.T) {
	c := newClock()
	cs := fake.NewClientset(lease("pg-0"))
	s := New(cs, "db", "pg", 15*time.Second)
	s.SetClock(c.now)
	r, err := s.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The renew time in the object is already "old" on our clock's terms,
	// but we only just saw it: not expired.
	c.add(10 * time.Second)
	if s.Expired(r) {
		t.Fatal("expired before a full duration was observed")
	}
	c.add(6 * time.Second)
	r, _ = s.Get(context.Background())
	if !s.Expired(r) {
		t.Fatal("not expired after a full unchanged duration")
	}
}

func TestEmptyHolderIsExpired(t *testing.T) {
	s := New(fake.NewClientset(lease("")), "db", "pg", 15*time.Second)
	r, err := s.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !s.Expired(r) {
		t.Fatal("empty holder not expired")
	}
}

func TestAcquireRenewRelease(t *testing.T) {
	c := newClock()
	cs := fake.NewClientset(lease(""))
	enforceResourceVersion(cs)
	s := New(cs, "db", "pg", 15*time.Second)
	s.SetClock(c.now)
	ctx := context.Background()
	r, _ := s.Get(ctx)
	r, err := s.Acquire(ctx, r, "pg-1", map[string]string{SystemID: "42"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Holder != "pg-1" || r.Transitions != 1 || r.Ann(SystemID) != "42" {
		t.Fatalf("after acquire: %+v", r)
	}
	c.add(2 * time.Second)
	r, err = s.Renew(ctx, r, "pg-1", map[string]string{LSN: "0/3000000"})
	if err != nil {
		t.Fatal(err)
	}
	if !r.RenewTime.Equal(c.t) || r.Ann(LSN) != "0/3000000" || r.Ann(SystemID) != "42" {
		t.Fatalf("after renew: %+v", r)
	}
	if _, err := s.Renew(ctx, r, "pg-2", nil); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("renew by non-holder: %v", err)
	}
	r, err = s.Release(ctx, r, "pg-1", map[string]string{Successor: "pg-2", LSN: ""})
	if err != nil {
		t.Fatal(err)
	}
	if r.Holder != "" || r.Ann(Successor) != "pg-2" || r.Ann(LSN) != "" {
		t.Fatalf("after release: %+v", r)
	}
}

func TestAcquireLosesToConcurrentWriter(t *testing.T) {
	cs := fake.NewClientset(lease(""))
	enforceResourceVersion(cs)
	ctx := context.Background()
	a := New(cs, "db", "pg", 15*time.Second)
	b := New(cs, "db", "pg", 15*time.Second)
	ra, _ := a.Get(ctx)
	rb, _ := b.Get(ctx)
	if _, err := a.Acquire(ctx, ra, "pg-0", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Acquire(ctx, rb, "pg-1", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("second acquire on a stale read: %v", err)
	}
}

func TestAnnotate(t *testing.T) {
	cs := fake.NewClientset(lease("pg-0"))
	s := New(cs, "db", "pg", 15*time.Second)
	ctx := context.Background()
	if err := s.Annotate(ctx, map[string]string{Switchover: "any"}); err != nil {
		t.Fatal(err)
	}
	r, _ := s.Get(ctx)
	if r.Ann(Switchover) != "any" || r.Holder != "pg-0" {
		t.Fatalf("after annotate: %+v", r)
	}
	if err := s.Annotate(ctx, map[string]string{Switchover: ""}); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Get(ctx)
	if _, ok := r.Annotations[Switchover]; ok {
		t.Fatal("annotation not removed")
	}
}
