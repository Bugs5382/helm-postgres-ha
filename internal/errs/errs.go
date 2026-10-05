// Package errs holds the agent's coded errors. Each code is stable, appears in
// the logs as the error's "code" field, and is listed in docs/errors.md, so an
// operator can quote it and look it up.
package errs

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
	apperr "github.com/Bugs5382/go-apperr"
)

// Codes. The 11xx range belongs to the agent.
const (
	Config          = 1101
	SystemIDMatch   = 1102
	MajorVersion    = 1103
	PeerTLS         = 1104
	LeaseUnreadable = 1105
	Promote         = 1106
	Rewind          = 1107
	Clone           = 1108
	Bootstrap       = 1109
	Backup          = 1110
	Fenced          = 1111
	Roles           = 1112
	ArchiveInUse    = 1114
)

var entries = []apperr.Entry{
	{Code: Config, Title: "config", Cause: "the agent's environment is missing a setting or holds an invalid one", Category: apperr.CategoryInvalid},
	{Code: SystemIDMatch, Title: "bootstrap", Cause: "the data directory belongs to a different cluster than the one recorded on the Lease; the member refuses to start", Category: apperr.CategoryInvalid},
	{Code: MajorVersion, Title: "upgrade", Cause: "the data directory was created by a different PostgreSQL major version than the server image", Category: apperr.CategoryInvalid},
	{Code: PeerTLS, Title: "tls", Cause: "the peer certificate, key or CA bundle is missing or invalid; the agent does not run without mutual TLS", Category: apperr.CategoryInvalid},
	{Code: LeaseUnreadable, Title: "lease", Cause: "the cluster Lease could not be read or written", Category: apperr.CategoryUnavailable},
	{Code: Promote, Title: "failover", Cause: "the standby holding the Lease did not finish promotion in time; the Lease was released", Category: apperr.CategoryUnavailable},
	{Code: Rewind, Title: "rejoin", Cause: "pg_rewind could not resynchronise a former primary; its data was moved aside and it is re-cloned", Category: apperr.CategoryInternal},
	{Code: Clone, Title: "rejoin", Cause: "cloning a standby from the primary failed; it is retried", Category: apperr.CategoryUnavailable},
	{Code: Bootstrap, Title: "bootstrap", Cause: "creating or restoring the cluster's first data failed", Category: apperr.CategoryInternal},
	{Code: Backup, Title: "backup", Cause: "a WAL-G base backup or its retention run failed", Category: apperr.CategoryUnavailable},
	{Code: Fenced, Title: "fencing", Cause: "the primary could not renew the Lease in time and stopped PostgreSQL so it cannot accept writes", Category: apperr.CategoryUnavailable},
	{Code: Roles, Title: "roles", Cause: "creating or updating roles and databases on the primary failed; it is retried", Category: apperr.CategoryInternal},
	{Code: ArchiveInUse, Title: "bootstrap", Cause: "the backup storage prefix already holds another cluster's backups or WAL; a new cluster refuses to archive into it until the prefix is changed or emptied", Category: apperr.CategoryInvalid},
}

// Registry returns the agent's registry.
func Registry() *apperr.Registry {
	reg, err := apperr.NewRegistry(entries, apperr.WithService(11), apperr.WithCodeDigits(4))
	if err != nil {
		// The table is a compile-time constant; a bad entry is a programming
		// error caught by the package tests.
		panic(err)
	}
	return reg
}

// New wraps err with a code.
func New(code int, err error) error { return apperr.Coded(code, err) }

// Code returns an error's code, or 0.
func Code(err error) int {
	c, _ := apperr.Code(err)
	return c
}
