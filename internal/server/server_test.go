package server

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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Bugs5382/helm-postgres-ha/internal/peer"
)

type member struct {
	live  error
	ready bool
	fresh bool
}

func (m member) Live() error         { return m.live }
func (m member) Ready() bool         { return m.ready }
func (m member) Status() peer.Status { return peer.Status{Pod: "pg-0", Role: peer.RoleStandby} }
func (m member) Fresh() bool         { return m.fresh }

func get(h http.Handler, path string) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code
}

func TestHealth(t *testing.T) {
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	ok := HealthHandler(member{ready: true}, metrics)
	if get(ok, "/livez") != 200 || get(ok, "/readyz") != 200 || get(ok, "/metrics") != 200 {
		t.Fatal("healthy member not reported healthy")
	}
	bad := HealthHandler(member{live: errors.New("stalled")}, metrics)
	if get(bad, "/livez") != 503 || get(bad, "/readyz") != 503 {
		t.Fatal("stalled member reported healthy")
	}
}

func TestPeerHandlerRefusesStaleStatus(t *testing.T) {
	if get(PeerHandler(member{fresh: true}), peer.StatusPath) != 200 {
		t.Fatal("fresh status refused")
	}
	if get(PeerHandler(member{fresh: false}), peer.StatusPath) != 503 {
		t.Fatal("stale status served")
	}
}
