// Package lease keeps the cluster's single source of truth: a
// coordination.k8s.io Lease whose holder is the primary. Its annotations carry
// the cluster's system identifier and the primary's last WAL position.
//
// Expiry is judged the way client-go's leader election judges it: by how long
// ago this process saw the holder or renew time change, on its own monotonic
// clock, never by comparing wall clocks across nodes.
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
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"sync"
	"time"

	coordv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Annotation keys on the cluster Lease.
const (
	Prefix = "postgres-ha/"
	// SystemID is the cluster's database system identifier, set once by the
	// member that bootstraps it.
	SystemID = Prefix + "system-identifier"
	// Timeline and LSN are the primary's position, refreshed on every renew.
	Timeline = Prefix + "timeline"
	LSN      = Prefix + "lsn"
	// Revision is the primary's StatefulSet controller revision.
	Revision = Prefix + "revision"
	// Successor names the member a departing primary handed over to.
	Successor = Prefix + "successor"
	// SuccessorAt is when the handover was written (RFC 3339).
	SuccessorAt = Prefix + "successor-at"
	// Switchover asks the primary to hand over: a member name, or "any".
	Switchover = Prefix + "switchover"
)

// Record is a snapshot of the Lease.
type Record struct {
	Holder      string
	RenewTime   time.Time
	Transitions int32
	Annotations map[string]string

	obj *coordv1.Lease
}

// Ann returns one annotation.
func (r Record) Ann(key string) string { return r.Annotations[key] }

// ErrConflict means another writer updated the Lease first.
var ErrConflict = errors.New("lease was updated by someone else")

// ErrNotHolder means a renew or release was attempted by a member that does
// not hold the Lease.
var ErrNotHolder = errors.New("not the lease holder")

// Store reads and writes one Lease.
type Store struct {
	client   kubernetes.Interface
	ns, name string
	duration time.Duration
	now      func() time.Time

	mu       sync.Mutex
	seen     bool
	obsKey   string
	obsAt    time.Time
	lastRead Record
}

// New returns a Store for the Lease ns/name with the given lease duration.
func New(client kubernetes.Interface, ns, name string, duration time.Duration) *Store {
	return &Store{client: client, ns: ns, name: name, duration: duration, now: time.Now}
}

// SetClock replaces the clock; tests use it.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Name is the Lease name.
func (s *Store) Name() string { return s.name }

func toRecord(l *coordv1.Lease) Record {
	r := Record{Annotations: map[string]string{}, obj: l}
	maps.Copy(r.Annotations, l.Annotations)
	if l.Spec.HolderIdentity != nil {
		r.Holder = *l.Spec.HolderIdentity
	}
	if l.Spec.RenewTime != nil {
		r.RenewTime = l.Spec.RenewTime.Time
	}
	if l.Spec.LeaseTransitions != nil {
		r.Transitions = *l.Spec.LeaseTransitions
	}
	return r
}

func key(r Record) string {
	return r.Holder + "@" + r.RenewTime.UTC().Format(time.RFC3339Nano)
}

// observe records when this process first saw the holder/renew pair.
func (s *Store) observe(r Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k := key(r); !s.seen || k != s.obsKey {
		s.seen, s.obsKey, s.obsAt = true, k, s.now()
	}
	s.lastRead = r
}

// Get reads the Lease.
func (s *Store) Get(ctx context.Context) (Record, error) {
	l, err := s.client.CoordinationV1().Leases(s.ns).Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		return Record{}, fmt.Errorf("get lease %s/%s: %w", s.ns, s.name, err)
	}
	r := toRecord(l)
	s.observe(r)
	return r, nil
}

// Expired reports whether r has no holder, or its holder has not renewed for
// a full lease duration as observed by this process.
func (s *Store) Expired(r Record) bool {
	if r.Holder == "" {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.seen || s.obsKey != key(r) {
		return false
	}
	return s.now().Sub(s.obsAt) > s.duration
}

// ObservedFor returns how long the current holder/renew pair has been
// unchanged as seen by this process.
func (s *Store) ObservedFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.seen {
		return 0
	}
	return s.now().Sub(s.obsAt)
}

func (s *Store) update(ctx context.Context, r Record, mutate func(*coordv1.Lease)) (Record, error) {
	if r.obj == nil {
		return Record{}, errors.New("record was not read from the API")
	}
	l := r.obj.DeepCopy()
	if l.Annotations == nil {
		l.Annotations = map[string]string{}
	}
	mutate(l)
	out, err := s.client.CoordinationV1().Leases(s.ns).Update(ctx, l, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) {
		return Record{}, ErrConflict
	}
	if err != nil {
		return Record{}, fmt.Errorf("update lease %s/%s: %w", s.ns, s.name, err)
	}
	nr := toRecord(out)
	s.observe(nr)
	return nr, nil
}

func merge(l *coordv1.Lease, ann map[string]string) {
	for k, v := range ann {
		if v == "" {
			delete(l.Annotations, k)
		} else {
			l.Annotations[k] = v
		}
	}
}

// Acquire takes the Lease for me. It fails with ErrConflict when someone else
// changed it since r was read, which is what makes the election safe.
func (s *Store) Acquire(ctx context.Context, r Record, me string, ann map[string]string) (Record, error) {
	now := metav1.NewMicroTime(s.now())
	secs := int32(min(s.duration/time.Second, math.MaxInt32)) // #nosec G115 -- bounded to MaxInt32 on the same line
	return s.update(ctx, r, func(l *coordv1.Lease) {
		if l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != me {
			t := r.Transitions + 1
			l.Spec.LeaseTransitions = &t
			l.Spec.AcquireTime = &now
		}
		l.Spec.HolderIdentity = &me
		l.Spec.RenewTime = &now
		l.Spec.LeaseDurationSeconds = &secs
		merge(l, ann)
	})
}

// Renew extends the holder's term.
func (s *Store) Renew(ctx context.Context, r Record, me string, ann map[string]string) (Record, error) {
	if r.Holder != me {
		return Record{}, ErrNotHolder
	}
	now := metav1.NewMicroTime(s.now())
	return s.update(ctx, r, func(l *coordv1.Lease) {
		l.Spec.RenewTime = &now
		merge(l, ann)
	})
}

// Release gives the Lease up so a successor does not wait for it to expire.
func (s *Store) Release(ctx context.Context, r Record, me string, ann map[string]string) (Record, error) {
	if r.Holder != me {
		return Record{}, ErrNotHolder
	}
	now := metav1.NewMicroTime(s.now())
	empty := ""
	return s.update(ctx, r, func(l *coordv1.Lease) {
		l.Spec.HolderIdentity = &empty
		l.Spec.RenewTime = &now
		merge(l, ann)
	})
}

// Annotate merges annotations without taking part in the election (an empty
// value removes a key). It is used for requests such as a switchover.
func (s *Store) Annotate(ctx context.Context, ann map[string]string) error {
	patch := map[string]any{}
	for k, v := range ann {
		if v == "" {
			patch[k] = nil
		} else {
			patch[k] = v
		}
	}
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": patch}})
	if err != nil {
		return err
	}
	_, err = s.client.CoordinationV1().Leases(s.ns).Patch(ctx, s.name, types.MergePatchType, body, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("annotate lease %s/%s: %w", s.ns, s.name, err)
	}
	return nil
}
