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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Backend names a WAL-G storage backend.
const (
	StorageS3   = "s3"
	StorageFile = "file"
)

var prefixVars = []string{"WALG_S3_PREFIX", "WALG_FILE_PREFIX"}

// Source is where a restore reads base backups and WAL from. An empty Prefix
// means the cluster's own storage, as configured in its environment.
type Source struct {
	Storage string
	Prefix  string
}

func (s Source) prefixVar() string {
	if s.Storage == StorageFile {
		return "WALG_FILE_PREFIX"
	}
	return "WALG_S3_PREFIX"
}

// Env returns base with the cluster's own WAL-G prefix replaced by the
// source's. WAL-G picks one storage by which prefix variable is set, so the
// other one must not be left behind.
func (s Source) Env(base []string) []string {
	if s.Prefix == "" {
		return base
	}
	out := make([]string, 0, len(base)+1)
	for _, e := range base {
		k, _, _ := strings.Cut(e, "=")
		if k == prefixVars[0] || k == prefixVars[1] {
			continue
		}
		out = append(out, e)
	}
	return append(out, s.prefixVar()+"="+s.Prefix)
}

// ShellPrefix is the same change for a shell command line, such as
// restore_command: empty for the cluster's own storage.
func (s Source) ShellPrefix() string {
	if s.Prefix == "" {
		return ""
	}
	return "env -u " + prefixVars[0] + " -u " + prefixVars[1] + " " + s.prefixVar() + "=" + shellQuote(s.Prefix) + " "
}

func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// SourceBackups lists the base backups in a restore source. A file source
// whose directory is missing is reported as unreachable rather than empty.
func (l *Local) SourceBackups(ctx context.Context, src Source) ([]string, error) {
	if src.Storage == StorageFile && src.Prefix != "" {
		if st, err := os.Stat(src.Prefix); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("restore source %s is not a reachable directory", src.Prefix)
		}
	}
	res, err := l.runner.Run(ctx, src.Env(l.cfg.Env), l.cfg.WalG, "backup-list", "--json")
	if err != nil {
		return nil, fmt.Errorf("list the backups in the restore source: %w", err)
	}
	return backupNames(res.Stdout)
}

func backupNames(out string) ([]string, error) {
	out = strings.TrimSpace(out)
	if out == "" || out == "null" {
		return nil, nil
	}
	var items []struct {
		Name string `json:"backup_name"`
	}
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		return nil, fmt.Errorf("unexpected backup-list output: %w", err)
	}
	names := make([]string, 0, len(items))
	for _, it := range items {
		names = append(names, it.Name)
	}
	return names, nil
}
