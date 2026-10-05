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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bugs5382/helm-postgres-ha/internal/cluster"
)

var topo = cluster.Topology{Cluster: "pg", Namespace: "db", Headless: "pg-headless", Replicas: 3}

type ca struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newCA(t *testing.T) ca {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return ca{cert: c, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue writes a leaf certificate for names into dir, with the CA bundle.
func (a ca) issue(t *testing.T, dir string, serial int64, names ...string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: names[0]},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: names, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	write := func(name string, b []byte) {
		// Write through a rename, the way a mounted Secret updates.
		tmp := filepath.Join(dir, "."+name)
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	write("tls.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	write("tls.key", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}))
	write("ca.crt", a.pem)
}

func material(t *testing.T, dir string) *Material {
	t.Helper()
	m, err := NewMaterial(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// serve starts the status API with the server material in dir and returns the
// listener address.
func serve(t *testing.T, dir string) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", ServerTLS(material(t, dir), topo))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: Handler(func() (Status, bool) { return Status{Pod: "pg-1", Role: RoleStandby, Position: 42}, true }), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func client(t *testing.T, dir, addr string) *Client {
	t.Helper()
	c := NewClient(material(t, dir), topo, "8009", 2*time.Second)
	c.redirect = func(string) string { return addr }
	return c
}

func TestStatusOverMutualTLS(t *testing.T) {
	a := newCA(t)
	srvDir, cliDir := t.TempDir(), t.TempDir()
	a.issue(t, srvDir, 2, "*.pg-headless.db.svc")
	a.issue(t, cliDir, 3, "*.pg-headless.db.svc")
	s, err := client(t, cliDir, serve(t, srvDir)).Fetch(context.Background(), "pg-1")
	if err != nil {
		t.Fatal(err)
	}
	if s.Pod != "pg-1" || s.Position != 42 {
		t.Fatalf("status = %+v", s)
	}
}

func TestRefusesClientFromAnotherNamespace(t *testing.T) {
	a := newCA(t)
	srvDir, cliDir := t.TempDir(), t.TempDir()
	a.issue(t, srvDir, 2, "*.pg-headless.db.svc")
	// Same CA, same pod name, another namespace.
	a.issue(t, cliDir, 3, "pg-1.pg-headless.other.svc")
	if _, err := client(t, cliDir, serve(t, srvDir)).Fetch(context.Background(), "pg-1"); err == nil {
		t.Fatal("client certificate for another namespace was accepted")
	}
}

func TestRefusesClientFromAnotherCA(t *testing.T) {
	a, b := newCA(t), newCA(t)
	srvDir, cliDir := t.TempDir(), t.TempDir()
	a.issue(t, srvDir, 2, "*.pg-headless.db.svc")
	b.issue(t, cliDir, 3, "*.pg-headless.db.svc")
	// The client trusts the server's CA, but presents a foreign certificate.
	if err := os.WriteFile(filepath.Join(cliDir, "ca.crt"), a.pem, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := client(t, cliDir, serve(t, srvDir)).Fetch(context.Background(), "pg-1"); err == nil {
		t.Fatal("certificate from another CA was accepted")
	}
}

func TestRefusesPlaintextClient(t *testing.T) {
	a := newCA(t)
	srvDir := t.TempDir()
	a.issue(t, srvDir, 2, "*.pg-headless.db.svc")
	addr := serve(t, srvDir)
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + addr + StatusPath)
	if err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatal("plaintext request was served")
		}
	}
	// A TLS client without a certificate is refused too.
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}) // #nosec G402 -- test client
	if err == nil {
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		_, werr := conn.Write([]byte("GET " + StatusPath + " HTTP/1.1\r\nHost: x\r\n\r\n"))
		buf := make([]byte, 64)
		_, rerr := conn.Read(buf)
		_ = conn.Close()
		if werr == nil && rerr == nil {
			t.Fatal("TLS client without a certificate was served")
		}
	}
}

func TestClientRefusesWrongServerName(t *testing.T) {
	a := newCA(t)
	srvDir, cliDir := t.TempDir(), t.TempDir()
	a.issue(t, srvDir, 2, "pg-1.pg-headless.other.svc")
	a.issue(t, cliDir, 3, "*.pg-headless.db.svc")
	_, err := client(t, cliDir, serve(t, srvDir)).Fetch(context.Background(), "pg-1")
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("err = %v", err)
	}
}

func TestReloadsRotatedCertificates(t *testing.T) {
	a, b := newCA(t), newCA(t)
	srvDir, cliDir := t.TempDir(), t.TempDir()
	a.issue(t, srvDir, 2, "*.pg-headless.db.svc")
	a.issue(t, cliDir, 3, "*.pg-headless.db.svc")
	addr := serve(t, srvDir)
	c := client(t, cliDir, addr)
	if _, err := c.Fetch(context.Background(), "pg-1"); err != nil {
		t.Fatal(err)
	}
	// Rotate both sides to a new CA; the running server and client must
	// pick it up without being rebuilt.
	time.Sleep(10 * time.Millisecond)
	b.issue(t, srvDir, 4, "*.pg-headless.db.svc")
	b.issue(t, cliDir, 5, "*.pg-headless.db.svc")
	c.http.CloseIdleConnections()
	if _, err := c.Fetch(context.Background(), "pg-1"); err != nil {
		t.Fatalf("after rotation: %v", err)
	}
}

func TestFetchAllReportsUnreachable(t *testing.T) {
	a := newCA(t)
	srvDir, cliDir := t.TempDir(), t.TempDir()
	a.issue(t, srvDir, 2, "*.pg-headless.db.svc")
	a.issue(t, cliDir, 3, "*.pg-headless.db.svc")
	addr := serve(t, srvDir)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	_ = ln.Close()
	c := NewClient(material(t, cliDir), topo, "8009", time.Second)
	c.redirect = func(a string) string {
		if strings.HasPrefix(a, "pg-1.") {
			return addr
		}
		return dead
	}
	res := c.FetchAll(context.Background(), []string{"pg-1", "pg-2"})
	if res["pg-1"].Err != nil || res["pg-2"].Err == nil {
		t.Fatalf("results = %+v", res)
	}
}

func TestMaterialRequiresFiles(t *testing.T) {
	if _, err := NewMaterial("/nope/tls.crt", "/nope/tls.key", "/nope/ca.crt"); err == nil {
		t.Fatal("missing material accepted")
	}
}

// TestFetchAllIsBoundedByOneTimeout: two members that accept a connection and
// never answer (a hung process, a blackholed node) must not stretch a round
// past one peer timeout, however many there are.
func TestFetchAllIsBoundedByOneTimeout(t *testing.T) {
	a := newCA(t)
	cliDir := t.TempDir()
	a.issue(t, cliDir, 3, "*.pg-headless.db.svc")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept and say nothing.
			defer func() { _ = c.Close() }()
		}
	}()
	timeout := 500 * time.Millisecond
	c := NewClient(material(t, cliDir), topo, "8009", timeout)
	c.redirect = func(string) string { return ln.Addr().String() }
	start := time.Now()
	res := c.FetchAll(context.Background(), []string{"pg-0", "pg-1", "pg-2"})
	elapsed := time.Since(start)
	for m, r := range res {
		if r.Err == nil {
			t.Errorf("%s answered", m)
		}
	}
	if elapsed > timeout+300*time.Millisecond {
		t.Fatalf("a round with three hung members took %s, want about one %s timeout", elapsed, timeout)
	}
}
