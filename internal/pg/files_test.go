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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLSN(t *testing.T) {
	l, err := ParseLSN("16/B374D848")
	if err != nil {
		t.Fatal(err)
	}
	if uint64(l) != 0x16B374D848 || l.String() != "16/B374D848" {
		t.Fatalf("lsn = %x %s", uint64(l), l)
	}
	if z, err := ParseLSN(""); err != nil || z != 0 {
		t.Fatalf("empty = %v %v", z, err)
	}
	for _, bad := range []string{"16", "x/1", "1/zz"} {
		if _, err := ParseLSN(bad); err == nil {
			t.Errorf("ParseLSN(%q) accepted", bad)
		}
	}
}

func TestRenderConfQuotesAndSorts(t *testing.T) {
	got := string(RenderConf(map[string]string{
		"primary_slot_name": "pg_1",
		"primary_conninfo":  "host=a application_name='x'",
	}))
	want := "primary_conninfo = 'host=a application_name=''x'''\nprimary_slot_name = 'pg_1'\n"
	if !strings.HasSuffix(got, want) {
		t.Fatalf("conf =\n%s", got)
	}
}

func TestWriteAgentConfReportsChange(t *testing.T) {
	dir := t.TempDir()
	s := map[string]string{"a": "1"}
	if changed, err := WriteAgentConf(dir, s); err != nil || !changed {
		t.Fatalf("first write: %v %v", changed, err)
	}
	if changed, err := WriteAgentConf(dir, s); err != nil || changed {
		t.Fatalf("same write: %v %v", changed, err)
	}
	st, err := os.Stat(filepath.Join(dir, AgentConf))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v %v", st.Mode(), err)
	}
	s["a"] = "2"
	if changed, err := WriteAgentConf(dir, s); err != nil || !changed {
		t.Fatalf("changed write: %v %v", changed, err)
	}
}

func TestSignalsAndData(t *testing.T) {
	dir := t.TempDir()
	if ok, err := HasData(dir); err != nil || ok {
		t.Fatalf("empty dir has data: %v %v", ok, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("18\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, _ := HasData(dir); !ok {
		t.Fatal("no data seen")
	}
	if m, err := DataMajor(dir); err != nil || m != 18 {
		t.Fatalf("major = %d %v", m, err)
	}
	if err := SetSignal(dir, StandbySignal, true); err != nil || !HasSignal(dir, StandbySignal) {
		t.Fatalf("signal not set: %v", err)
	}
	if err := SetSignal(dir, StandbySignal, false); err != nil || HasSignal(dir, StandbySignal) {
		t.Fatalf("signal not removed: %v", err)
	}
	if err := SetSignal(dir, StandbySignal, false); err != nil {
		t.Fatalf("removing a missing signal: %v", err)
	}
}

func TestPassFileEscapes(t *testing.T) {
	got := string(RenderPassFile([]PassEntry{{User: "replicator", Password: `a:b\c`}}))
	if got != "*:*:*:replicator:a\\:b\\\\c\n" {
		t.Fatalf("pgpass = %q", got)
	}
}

func TestReadSecret(t *testing.T) {
	p := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(p, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := ReadSecret(p); err != nil || s != "s3cret" {
		t.Fatalf("secret = %q %v", s, err)
	}
	if err := os.WriteFile(p, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecret(p); err == nil {
		t.Fatal("empty secret accepted")
	}
}

func TestScramVerifierMatchesReference(t *testing.T) {
	salt := make([]byte, 16)
	for i := range salt {
		salt[i] = byte(i)
	}
	got, err := scramVerifier("pencil", salt, 4096)
	if err != nil {
		t.Fatal(err)
	}
	// Computed independently with Python's hashlib and hmac.
	want := "SCRAM-SHA-256$4096:AAECAwQFBgcICQoLDA0ODw==$zHCdol2044/ZyWzPLi7oxApCkamKw9Z+E4U/QApd/5Y=:dd5peBOitVnLNFu7VmwP+HiDaaw4OUCv396eVCWhYiE="
	if got != want {
		t.Fatalf("verifier =\n%s\nwant\n%s", got, want)
	}
}

func TestScramMatches(t *testing.T) {
	v, err := ScramVerifier("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !ScramMatches(v, "hunter2") {
		t.Fatal("own verifier does not match")
	}
	if ScramMatches(v, "hunter3") {
		t.Fatal("wrong password matches")
	}
	for _, bad := range []string{"", "md5abc", "SCRAM-SHA-256$x:AA==$a:b", "SCRAM-SHA-256$4096"} {
		if ScramMatches(bad, "hunter2") {
			t.Errorf("malformed verifier %q matches", bad)
		}
	}
}
