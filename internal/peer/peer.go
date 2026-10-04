// Package peer is the members' read-only status API over mutual TLS. A
// candidate asks every other member for its position before it takes the
// Lease, so it never promotes while another member still sees a primary or
// holds more WAL. The API changes nothing: only the Lease holder ever
// promotes, and only its own server.
package peer

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
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/Bugs5382/helm-postgres-ha/internal/cluster"
)

// StatusPath is the one route the API serves.
const StatusPath = "/v1/status"

// Roles a member reports.
const (
	RolePrimary       = "primary"
	RoleStandby       = "standby"
	RoleStopped       = "stopped"
	RoleBootstrapping = "bootstrapping"
)

// Status is what a member reports about itself.
type Status struct {
	Pod      string `json:"pod"`
	Role     string `json:"role"`
	Running  bool   `json:"running"`
	HasData  bool   `json:"hasData"`
	SystemID string `json:"systemId,omitempty"`
	// InRecovery is true on a standby.
	InRecovery bool   `json:"inRecovery"`
	Timeline   int    `json:"timeline"`
	Position   uint64 `json:"position"`
	ReplayLSN  uint64 `json:"replayLsn"`
	// Streaming is true while the member's WAL receiver is attached to an
	// upstream.
	Streaming bool   `json:"streaming"`
	Upstream  string `json:"upstream,omitempty"`
	// Eligible is true when the member could be promoted right now.
	Eligible bool   `json:"eligible"`
	Revision string `json:"revision,omitempty"`
	// Busy names a long operation in progress (clone, rewind, restore).
	Busy string `json:"busy,omitempty"`
}

// Material is the member's certificate, key and CA bundle, read from the
// files the chart mounts and re-read whenever they change, so a rotated
// certificate is picked up without a restart.
type Material struct {
	certFile, keyFile, caFile string

	mu    sync.Mutex
	stamp string
	cert  *tls.Certificate
	pool  *x509.CertPool
}

// NewMaterial loads the material once and fails when it is missing, so the
// agent refuses to start without mTLS.
func NewMaterial(certFile, keyFile, caFile string) (*Material, error) {
	m := &Material{certFile: certFile, keyFile: keyFile, caFile: caFile}
	if _, _, err := m.current(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Material) fileStamp() (string, error) {
	s := ""
	for _, f := range []string{m.certFile, m.keyFile, m.caFile} {
		st, err := os.Stat(f)
		if err != nil {
			return "", err
		}
		s += f + ":" + strconv.FormatInt(st.ModTime().UnixNano(), 10) + ":" + strconv.FormatInt(st.Size(), 10) + ";"
	}
	return s, nil
}

// current returns the material, reloading it when a file changed. A failed
// reload keeps serving the last good material.
func (m *Material) current() (*tls.Certificate, *x509.CertPool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stamp, err := m.fileStamp()
	if err == nil && stamp == m.stamp && m.cert != nil {
		return m.cert, m.pool, nil
	}
	cert, pool, lerr := m.load()
	if lerr != nil {
		if m.cert != nil {
			return m.cert, m.pool, nil
		}
		return nil, nil, lerr
	}
	m.stamp, m.cert, m.pool = stamp, cert, pool
	return cert, pool, nil
}

func (m *Material) load() (*tls.Certificate, *x509.CertPool, error) {
	cert, err := tls.LoadX509KeyPair(m.certFile, m.keyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("load peer certificate: %w", err)
	}
	ca, err := os.ReadFile(m.caFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read peer CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, nil, errors.New("peer CA bundle holds no certificates")
	}
	return &cert, pool, nil
}

// verifyChain checks a presented chain against the current CA and returns the
// leaf.
func verifyChain(raw [][]byte, pool *x509.CertPool, usage x509.ExtKeyUsage) (*x509.Certificate, error) {
	if len(raw) == 0 {
		return nil, errors.New("no certificate presented")
	}
	certs := make([]*x509.Certificate, 0, len(raw))
	for _, r := range raw {
		c, err := x509.ParseCertificate(r)
		if err != nil {
			return nil, err
		}
		certs = append(certs, c)
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	if _, err := certs[0].Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
		return nil, err
	}
	return certs[0], nil
}

// isMember reports whether a certificate names a member of the topology.
func isMember(c *x509.Certificate, topo cluster.Topology) bool {
	for _, n := range c.DNSNames {
		if topo.IsMemberSAN(n) {
			return true
		}
	}
	return false
}

// ServerTLS is the server side: TLS 1.3, a client certificate from the
// cluster CA is required, and it must name a member of this cluster.
func ServerTLS(m *Material, topo cluster.Topology) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			cert, pool, err := m.current()
			if err != nil {
				return nil, err
			}
			return &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{*cert},
				ClientAuth:   tls.RequireAnyClientCert,
				VerifyConnection: func(cs tls.ConnectionState) error {
					raw := make([][]byte, 0, len(cs.PeerCertificates))
					for _, c := range cs.PeerCertificates {
						raw = append(raw, c.Raw)
					}
					leaf, err := verifyChain(raw, pool, x509.ExtKeyUsageClientAuth)
					if err != nil {
						return fmt.Errorf("peer client certificate: %w", err)
					}
					if !isMember(leaf, topo) {
						return fmt.Errorf("peer client certificate %v does not name a member of this cluster", leaf.DNSNames)
					}
					return nil
				},
			}, nil
		},
	}
}

// clientTLS dials one member: its certificate must chain to the current CA
// and carry the member's full host name (or the cluster wildcard).
func clientTLS(m *Material, topo cluster.Topology, host string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: host,
		// The chain and name are verified in VerifyConnection against the CA
		// as it is now, so a rotated CA takes effect without a restart.
		InsecureSkipVerify: true, // #nosec G402 -- verification happens in VerifyConnection
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, _, err := m.current()
			return cert, err
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			_, pool, err := m.current()
			if err != nil {
				return err
			}
			raw := make([][]byte, 0, len(cs.PeerCertificates))
			for _, c := range cs.PeerCertificates {
				raw = append(raw, c.Raw)
			}
			leaf, err := verifyChain(raw, pool, x509.ExtKeyUsageServerAuth)
			if err != nil {
				return fmt.Errorf("peer server certificate: %w", err)
			}
			if err := leaf.VerifyHostname(host); err != nil {
				return fmt.Errorf("peer server certificate: %w", err)
			}
			if !isMember(leaf, topo) {
				return fmt.Errorf("peer server certificate %v does not name a member of this cluster", leaf.DNSNames)
			}
			return nil
		},
	}
}

// Handler serves the status API. status reports false when the member's
// loop has stalled; peers then get 503 rather than an old answer.
func Handler(status func() (Status, bool)) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+StatusPath, func(w http.ResponseWriter, _ *http.Request) {
		s, fresh := status()
		if !fresh {
			http.Error(w, "status is stale", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s)
	})
	return mux
}

// Result is one member's answer, or why there was none.
type Result struct {
	Status Status
	Err    error
}

// Client asks members for their status.
type Client struct {
	// redirect rewrites the dial address; tests point member names at a local
	// listener with it.
	redirect func(addr string) string
	topo     cluster.Topology
	port     string
	timeout  time.Duration
	http     *http.Client
}

// NewClient returns a Client that dials members on port.
func NewClient(m *Material, topo cluster.Topology, port string, timeout time.Duration) *Client {
	c := &Client{topo: topo, port: port, timeout: timeout, redirect: func(a string) string { return a }}
	tr := &http.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			d := tls.Dialer{NetDialer: &net.Dialer{Timeout: timeout}, Config: clientTLS(m, topo, host)}
			return d.DialContext(ctx, network, c.redirect(addr))
		},
		MaxIdleConnsPerHost: 1,
		IdleConnTimeout:     30 * time.Second,
		ForceAttemptHTTP2:   false,
	}
	c.http = &http.Client{Transport: tr, Timeout: timeout}
	return c
}

// Fetch asks one member.
func (c *Client) Fetch(ctx context.Context, pod string) (Status, error) {
	url := "https://" + net.JoinHostPort(c.topo.Host(pod), c.port) + StatusPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Status{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Status{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Status{}, fmt.Errorf("status %d from %s", resp.StatusCode, pod)
	}
	var s Status
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&s); err != nil {
		return Status{}, fmt.Errorf("decode status from %s: %w", pod, err)
	}
	if s.Pod != pod {
		return Status{}, fmt.Errorf("member %s answered as %q", pod, s.Pod)
	}
	return s, nil
}

// FetchAll asks every listed member at once, within one timeout.
func (c *Client) FetchAll(ctx context.Context, pods []string) map[string]Result {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	out := make(map[string]Result, len(pods))
	for _, p := range pods {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := c.Fetch(ctx, p)
			mu.Lock()
			out[p] = Result{Status: s, Err: err}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}
