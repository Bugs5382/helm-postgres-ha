// Package pooler resets PgBouncer after a promotion. A pooler keeps its
// server connections to the old primary, and when that server hangs instead
// of dying (a frozen process, an I/O stall) its kernel still acknowledges
// TCP, so neither keepalives nor tcp_user_timeout ever drop them. The new
// primary therefore tells every PgBouncer pod, over its admin console, to
// KILL each database's connections and RESUME it; new server connections go
// through the primary Service to the new primary.
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
	"fmt"
	"regexp"
	"sync"

	golog "github.com/Bugs5382/go-log"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// adminDatabase is PgBouncer's own console, never reset.
const adminDatabase = "pgbouncer"

// Endpoints lists the PgBouncer pods' addresses.
type Endpoints interface {
	Addresses(ctx context.Context) ([]string, error)
}

// Admin talks to one PgBouncer admin console.
type Admin interface {
	Databases(ctx context.Context, addr string) ([]string, error)
	Exec(ctx context.Context, addr, command string) error
}

// Resetter resets every pooler.
type Resetter struct {
	eps   Endpoints
	admin Admin
	log   golog.Logger
}

// New returns a Resetter.
func New(eps Endpoints, admin Admin, log golog.Logger) *Resetter {
	return &Resetter{eps: eps, admin: admin, log: log}
}

// The admin console takes bare identifiers; anything else is skipped.
var safeName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Reset drops every pooler's connections and returns how many poolers it
// reset. Poolers that cannot be reached (a pod on a dead node) are reported
// in the error and skipped; they have no connections that can be reused.
func (r *Resetter) Reset(ctx context.Context) (int, error) {
	addrs, err := r.eps.Addresses(ctx)
	if err != nil {
		return 0, fmt.Errorf("list poolers: %w", err)
	}
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		n    int
		errs []error
	)
	for _, addr := range addrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.resetOne(ctx, addr); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("pooler %s: %w", addr, err))
				mu.Unlock()
				return
			}
			mu.Lock()
			n++
			mu.Unlock()
		}()
	}
	wg.Wait()
	return n, errors.Join(errs...)
}

func (r *Resetter) resetOne(ctx context.Context, addr string) error {
	dbs, err := r.admin.Databases(ctx, addr)
	if err != nil {
		return err
	}
	for _, db := range dbs {
		if db == adminDatabase {
			continue
		}
		if !safeName.MatchString(db) {
			r.log.Warn("skipping a pooler database with an unusual name", golog.F("pooler", addr))
			continue
		}
		if err := r.admin.Exec(ctx, addr, "KILL "+db); err != nil {
			return fmt.Errorf("kill %s: %w", db, err)
		}
		if err := r.admin.Exec(ctx, addr, "RESUME "+db); err != nil {
			return fmt.Errorf("resume %s: %w", db, err)
		}
		r.log.Info("pooler connections dropped", golog.F("pooler", addr), golog.F("database", db))
	}
	return nil
}

// SliceEndpoints reads the PgBouncer Service's EndpointSlices. Endpoints that
// are not ready are included: a pooler that is starting may already hold
// connections.
type SliceEndpoints struct {
	cs      kubernetes.Interface
	ns, svc string
}

// NewEndpoints returns the EndpointSlice lister for a Service.
func NewEndpoints(cs kubernetes.Interface, ns, svc string) *SliceEndpoints {
	return &SliceEndpoints{cs: cs, ns: ns, svc: svc}
}

// Addresses lists every endpoint address of the Service.
func (s *SliceEndpoints) Addresses(ctx context.Context) ([]string, error) {
	list, err := s.cs.DiscoveryV1().EndpointSlices(s.ns).List(ctx, metav1.ListOptions{LabelSelector: discoveryv1.LabelServiceName + "=" + s.svc})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, sl := range list.Items {
		for _, ep := range sl.Endpoints {
			for _, a := range ep.Addresses {
				if !seen[a] {
					seen[a] = true
					out = append(out, a)
				}
			}
		}
	}
	return out, nil
}
