// Package pg controls the local PostgreSQL server: the postmaster process,
// the files in the data directory the agent owns, the server tools (initdb,
// pg_basebackup, pg_rewind, pg_controldata) and the SQL the agent runs over the
// local socket.
package pg

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
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// AgentConf is the file in the data directory the agent owns. The chart's
// postgresql.conf includes it last, so its settings win.
const AgentConf = "pgha.conf"

// LSN is a WAL position.
type LSN uint64

// ParseLSN parses the X/Y text form. An empty string is the zero LSN.
func ParseLSN(s string) (LSN, error) {
	if s == "" {
		return 0, nil
	}
	hi, lo, ok := strings.Cut(s, "/")
	if !ok {
		return 0, fmt.Errorf("invalid LSN %q", s)
	}
	h, err := strconv.ParseUint(hi, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid LSN %q: %w", s, err)
	}
	l, err := strconv.ParseUint(lo, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid LSN %q: %w", s, err)
	}
	return LSN(h<<32 | l), nil
}

// String formats the LSN as X/Y.
func (l LSN) String() string {
	return fmt.Sprintf("%X/%X", uint64(l)>>32, uint64(l)&0xffffffff)
}

// HasData reports whether dir holds an initialised data directory.
func HasData(dir string) (bool, error) {
	_, err := os.Stat(filepath.Join(dir, "PG_VERSION"))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// DataMajor returns the major version recorded in a data directory.
func DataMajor(dir string) (int, error) {
	b, err := os.ReadFile(filepath.Join(dir, "PG_VERSION")) // #nosec G304 -- the configured data directory
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// QuoteValue quotes a setting value for postgresql.conf.
func QuoteValue(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

// RenderConf renders settings in a stable order.
func RenderConf(settings map[string]string) []byte {
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteString("# Written by the postgres-ha agent. Do not edit; it is rewritten on every role change.\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s = %s\n", k, QuoteValue(settings[k]))
	}
	return b.Bytes()
}

// WriteFileAtomic writes data to path through a temporary file and a rename,
// and reports whether the content changed.
func WriteFileAtomic(path string, data []byte, mode fs.FileMode) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) { // #nosec G304 -- agent-owned path
		return false, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return false, err
	}
	return true, nil
}

// WriteAgentConf writes the agent's settings into the data directory and
// reports whether they changed.
func WriteAgentConf(dataDir string, settings map[string]string) (bool, error) {
	return WriteFileAtomic(filepath.Join(dataDir, AgentConf), RenderConf(settings), 0o600)
}

// SetSignal creates or removes a signal file (standby.signal,
// recovery.signal) in the data directory.
func SetSignal(dataDir, name string, on bool) error {
	p := filepath.Join(dataDir, name)
	if on {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- fixed names in the data directory
		if err != nil {
			return err
		}
		return f.Close()
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// HasSignal reports whether a signal file exists.
func HasSignal(dataDir, name string) bool {
	_, err := os.Stat(filepath.Join(dataDir, name))
	return err == nil
}

// Signal file names.
const (
	StandbySignal  = "standby.signal"
	RecoverySignal = "recovery.signal"
)

// PassEntry is one line of a libpq password file.
type PassEntry struct {
	User     string
	Password string
}

// RenderPassFile renders a libpq password file that matches any host, port and
// database for each user.
func RenderPassFile(entries []PassEntry) []byte {
	esc := strings.NewReplacer(`\`, `\\`, `:`, `\:`)
	var b bytes.Buffer
	for _, e := range entries {
		fmt.Fprintf(&b, "*:*:*:%s:%s\n", esc.Replace(e.User), esc.Replace(e.Password))
	}
	return b.Bytes()
}

// ReadSecret reads a password file the chart mounts, trimming a trailing
// newline.
func ReadSecret(path string) (string, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- chart-mounted secret path
	if err != nil {
		return "", err
	}
	s := strings.TrimRight(string(b), "\r\n")
	if s == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return s, nil
}
