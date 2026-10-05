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
	"strings"
	"testing"
)

func TestFatalWatchPassesOutputThroughAndKeepsTheLastFatal(t *testing.T) {
	var out bytes.Buffer
	w := newFatalWatch(&out)
	in := "2026-10-05 17:45:12.627 GMT [11029] LOG:  starting archive recovery\n" +
		"2026-10-05 17:45:12.700 GMT [11029] FATAL:  recovery ended before configured recovery target was reached\n" +
		"2026-10-05 17:45:12.701 GMT [11029] HINT:  If this has occurred more than once some data might be corrupted\n"
	// Written in pieces that split lines, as a pipe delivers them.
	for _, part := range []string{in[:30], in[30:120], in[120:]} {
		if _, err := w.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	if out.String() != in {
		t.Fatalf("output not passed through unchanged:\n%q", out.String())
	}
	if got := w.Last(); got != "recovery ended before configured recovery target was reached" {
		t.Fatalf("last fatal = %q", got)
	}
}

func TestFatalWatchKeepsPanicsAndIgnoresOtherLevels(t *testing.T) {
	w := newFatalWatch(&bytes.Buffer{})
	_, _ = w.Write([]byte("x PANIC:  could not locate a valid checkpoint record at 0/19000028\n"))
	_, _ = w.Write([]byte("x ERROR:  relation does not exist\nx LOG:  checkpoint complete\n"))
	if got := w.Last(); got != "could not locate a valid checkpoint record at 0/19000028" {
		t.Fatalf("last fatal = %q", got)
	}
	w.Reset()
	if w.Last() != "" {
		t.Fatal("Reset did not clear the last fatal")
	}
}

func TestFatalWatchBoundsAnUnterminatedLine(t *testing.T) {
	w := newFatalWatch(&bytes.Buffer{})
	_, _ = w.Write([]byte(strings.Repeat("a", 3*maxFatalLine)))
	_, _ = w.Write([]byte("\nx FATAL:  after a long line\n"))
	if got := w.Last(); got != "after a long line" {
		t.Fatalf("last fatal = %q", got)
	}
	if n := len(w.partial); n != 0 {
		t.Fatalf("partial line still holds %d bytes", n)
	}
}
