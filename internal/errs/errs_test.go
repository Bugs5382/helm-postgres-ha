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
	"errors"
	"os"
	"strings"
	"testing"
)

func TestRegistryBuilds(t *testing.T) {
	reg := Registry()
	if !strings.Contains(reg.Markdown(), "1111") {
		t.Fatal("fencing code missing from the table")
	}
	err := New(Fenced, errors.New("renew timed out"))
	if Code(err) != Fenced || Code(errors.New("x")) != 0 {
		t.Fatal("code lost")
	}
}

// TestDocsListEveryCode keeps docs/errors.md in step with the table.
func TestDocsListEveryCode(t *testing.T) {
	b, err := os.ReadFile("../../docs/errors.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), strings.TrimSpace(Registry().Markdown())) {
		t.Fatalf("docs/errors.md is out of date; paste in:\n%s", Registry().Markdown())
	}
}
