package proc

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
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseStat(t *testing.T) {
	state, ppid, err := parseStat("42 (post gres) (x)) Z 1 42 42 0")
	if err != nil || state != 'Z' || ppid != 1 {
		t.Fatalf("parseStat = %c %d %v", state, ppid, err)
	}
	if _, _, err := parseStat("garbage"); err == nil {
		t.Fatal("garbage parsed")
	}
}

func TestRunCapturesOutputAndErrors(t *testing.T) {
	r := New()
	res, err := r.Run(context.Background(), nil, "/bin/sh", "-c", "echo out; echo err >&2")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "out" || strings.TrimSpace(res.Stderr) != "err" {
		t.Fatalf("output = %q / %q", res.Stdout, res.Stderr)
	}
	_, err = r.Run(context.Background(), nil, "/bin/sh", "-c", "echo boom >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestReapLeavesManagedChildren(t *testing.T) {
	r := New()
	cmd := exec.Command("/bin/true")
	if err := r.Start(cmd); err != nil {
		t.Fatal(err)
	}
	// Give the child time to exit and turn into a zombie.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !isZombie(r, cmd.Process.Pid) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := r.ReapOnce(); n != 0 {
		t.Fatalf("reaped %d managed children", n)
	}
	if err := r.Wait(cmd); err != nil {
		t.Fatalf("owner lost the exit status: %v", err)
	}
}

func TestReapCollectsUnmanagedZombies(t *testing.T) {
	r := New()
	// Started outside the Runner, so nobody owns its exit status.
	cmd := exec.Command("/bin/true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !isZombie(r, cmd.Process.Pid) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := r.ReapOnce(); n != 1 {
		t.Fatalf("reaped %d, want 1", n)
	}
}

func isZombie(r *Runner, pid int) bool {
	state, _, err := readStat(r.procDir + "/" + strconv.Itoa(pid) + "/stat")
	return err == nil && state == 'Z'
}
