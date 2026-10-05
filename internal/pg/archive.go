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
	"strings"
)

// ArchiveUsed reports whether the cluster's own WAL-G storage already holds
// base backups or WAL. A new cluster must not archive into storage that
// another cluster wrote, or the two clusters' timelines mix.
func (l *Local) ArchiveUsed(ctx context.Context) (bool, error) {
	for _, args := range [][]string{{"backup-list", "--json"}, {"wal-show", "--detailed-json"}} {
		res, err := l.runner.Run(ctx, l.cfg.Env, l.cfg.WalG, args...)
		if err != nil {
			return false, fmt.Errorf("wal-g %s: %w", args[0], err)
		}
		used, err := jsonListHasEntries(res.Stdout)
		if err != nil {
			return false, fmt.Errorf("wal-g %s: %w", args[0], err)
		}
		if used {
			return true, nil
		}
	}
	return false, nil
}

// jsonListHasEntries parses WAL-G's JSON list output; empty output, null and
// [] mean nothing is stored.
func jsonListHasEntries(out string) (bool, error) {
	out = strings.TrimSpace(out)
	if out == "" || out == "null" {
		return false, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		return false, fmt.Errorf("unexpected output: %w", err)
	}
	return len(items) > 0, nil
}
