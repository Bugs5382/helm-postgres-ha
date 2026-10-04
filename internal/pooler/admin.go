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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// ConsoleAdmin speaks to PgBouncer's admin console. The console only takes
// the simple query protocol, so it uses a one-shot pgconn connection rather
// than a pool.
type ConsoleAdmin struct {
	Port     uint16
	User     string
	Password func() (string, error)
	// CAFile verifies the pooler's certificate chain. Pods are dialled by IP,
	// which is not in the certificate, so the name is not checked.
	CAFile  string
	Timeout time.Duration
}

func (a ConsoleAdmin) connect(ctx context.Context, addr string) (*pgconn.PgConn, error) {
	pw, err := a.Password()
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(a.CAFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("pooler CA bundle holds no certificates")
	}
	cfg, err := pgconn.ParseConfig("dbname=pgbouncer sslmode=require")
	if err != nil {
		return nil, err
	}
	cfg.Host, cfg.Port, cfg.User, cfg.Password = addr, a.Port, a.User, pw
	cfg.ConnectTimeout = a.Timeout
	cfg.Fallbacks = nil
	cfg.TLSConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		// The chain is verified below against the cluster CA.
		InsecureSkipVerify: true, // #nosec G402 -- chain verified in VerifyConnection; the pod IP is not a SAN
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("pooler presented no certificate")
			}
			inter := x509.NewCertPool()
			for _, c := range cs.PeerCertificates[1:] {
				inter.AddCert(c)
			}
			_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter})
			return err
		},
	}
	cctx, cancel := context.WithTimeout(ctx, a.Timeout)
	defer cancel()
	return pgconn.ConnectConfig(cctx, cfg)
}

// Databases lists the pooler's databases (SHOW DATABASES).
func (a ConsoleAdmin) Databases(ctx context.Context, addr string) ([]string, error) {
	c, err := a.connect(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close(context.Background()) }()
	qctx, cancel := context.WithTimeout(ctx, a.Timeout)
	defer cancel()
	res, err := c.Exec(qctx, "SHOW DATABASES").ReadAll()
	if err != nil {
		return nil, err
	}
	if len(res) != 1 {
		return nil, fmt.Errorf("SHOW DATABASES returned %d results", len(res))
	}
	var out []string
	for _, row := range res[0].Rows {
		if len(row) > 0 {
			out = append(out, string(row[0]))
		}
	}
	return out, nil
}

// Exec runs one admin command.
func (a ConsoleAdmin) Exec(ctx context.Context, addr, command string) error {
	c, err := a.connect(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.Background()) }()
	qctx, cancel := context.WithTimeout(ctx, a.Timeout)
	defer cancel()
	_, err = c.Exec(qctx, command).ReadAll()
	return err
}
