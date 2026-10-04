// Package server is the agent's transport: the plain HTTP listener for the
// kubelet probes and Prometheus, and the mutual-TLS listener for the peer
// status API.
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
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"time"

	golog "github.com/Bugs5382/go-log"

	"github.com/Bugs5382/helm-postgres-ha/internal/peer"
)

// Member is what the handlers read from the agent.
type Member interface {
	Live() error
	Ready() bool
	Status() peer.Status
	Fresh() bool
}

// HealthHandler serves /livez, /readyz and /metrics.
//
// /livez answers whether the control loop is ticking, not whether PostgreSQL
// accepts connections, so a long crash recovery or clone never gets the
// container killed. /readyz is role-aware: a primary that holds the Lease, or
// a standby that streams within the lag limit.
func HealthHandler(m Member, metrics http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		if err := m.Live(); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !m.Ready() {
			http.Error(w, "not ready: "+m.Status().Role, http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /metrics", metrics)
	return mux
}

// Server is one listener.
type Server struct {
	name string
	srv  *http.Server
	ln   net.Listener
	log  golog.Logger
}

// Listen opens a plain listener.
func Listen(name, addr string, h http.Handler, log golog.Logger) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &Server{name: name, ln: ln, log: log, srv: &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}}, nil
}

// ListenTLS opens a TLS listener.
func ListenTLS(name, addr string, h http.Handler, cfg *tls.Config, log golog.Logger) (*Server, error) {
	ln, err := tls.Listen("tcp", addr, cfg)
	if err != nil {
		return nil, err
	}
	return &Server{name: name, ln: ln, log: log, srv: &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}}, nil
}

// Serve serves until Shutdown.
func (s *Server) Serve() {
	s.log.Info("listening", golog.F("listener", s.name), golog.F("addr", s.ln.Addr().String()))
	if err := s.srv.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		s.log.Error(err, "listener stopped", golog.F("listener", s.name))
	}
}

// Shutdown stops the listener.
func (s *Server) Shutdown(ctx context.Context) { _ = s.srv.Shutdown(ctx) }

// PeerHandler serves the status API with the freshness check.
func PeerHandler(m Member) http.Handler {
	return peer.Handler(func() (peer.Status, bool) { return m.Status(), m.Fresh() })
}
