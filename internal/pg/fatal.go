// MIT License
//
// Copyright (c) 2026 Shane & Contributors
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.

package pg

import (
	"bytes"
	"io"
	"strings"
	"sync"
)

// maxFatalLine bounds how much of one unterminated line is buffered.
const maxFatalLine = 4096

// RecoveryTargetNotReached is the message PostgreSQL exits with when archive
// recovery runs out of WAL before its recovery target.
const RecoveryTargetNotReached = "recovery ended before configured recovery target was reached"

// fatalWatch copies the postmaster's stderr to out unchanged and remembers
// the message of the last FATAL or PANIC line, so the agent can tell why the
// server exited.
type fatalWatch struct {
	out     io.Writer
	mu      sync.Mutex
	partial []byte
	last    string
}

func newFatalWatch(out io.Writer) *fatalWatch { return &fatalWatch{out: out} }

func (w *fatalWatch) Write(p []byte) (int, error) {
	n, err := w.out.Write(p)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		w.scan(string(w.partial[:i]))
		w.partial = w.partial[i+1:]
	}
	if len(w.partial) > maxFatalLine {
		// A line this long is not a FATAL message; drop it up to its end.
		w.partial = w.partial[:0]
	}
	return n, err
}

func (w *fatalWatch) scan(line string) {
	for _, level := range []string{"FATAL:", "PANIC:"} {
		if i := strings.Index(line, level); i >= 0 {
			w.last = strings.TrimSpace(line[i+len(level):])
			return
		}
	}
}

// Last returns the message of the last FATAL or PANIC line.
func (w *fatalWatch) Last() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last
}

// Reset forgets the last message, for a new postmaster.
func (w *fatalWatch) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.last, w.partial = "", w.partial[:0]
}
