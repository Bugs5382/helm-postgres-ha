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
	"slices"
	"testing"
)

func TestSourceEnvReplacesTheOwnPrefixWithTheSources(t *testing.T) {
	base := []string{"PGDATA=/d", "WALG_S3_PREFIX=s3://own/pg", "AWS_REGION=x"}
	got := Source{Storage: StorageFile, Prefix: "/restore-source/pg"}.Env(base)
	want := []string{"PGDATA=/d", "AWS_REGION=x", "WALG_FILE_PREFIX=/restore-source/pg"}
	if !slices.Equal(got, want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
	got = Source{Storage: StorageS3, Prefix: "s3://other/pg"}.Env([]string{"WALG_FILE_PREFIX=/backup/pg"})
	if !slices.Equal(got, []string{"WALG_S3_PREFIX=s3://other/pg"}) {
		t.Fatalf("env = %v", got)
	}
	if !slices.Equal(base[:1], []string{"PGDATA=/d"}) || len(base) != 3 {
		t.Fatal("Env changed its input")
	}
}

func TestSourceWithoutAPrefixIsTheClustersOwnStorage(t *testing.T) {
	base := []string{"WALG_S3_PREFIX=s3://own/pg"}
	if got := (Source{}).Env(base); !slices.Equal(got, base) {
		t.Fatalf("env = %v", got)
	}
	if got := (Source{}).ShellPrefix(); got != "" {
		t.Fatalf("shell prefix = %q", got)
	}
}

func TestSourceShellPrefixUnsetsBothAndSetsOne(t *testing.T) {
	got := Source{Storage: StorageFile, Prefix: "/restore-source/it's"}.ShellPrefix()
	want := `env -u WALG_S3_PREFIX -u WALG_FILE_PREFIX WALG_FILE_PREFIX='/restore-source/it'\''s' `
	if got != want {
		t.Fatalf("shell prefix = %q, want %q", got, want)
	}
	if got := (Source{Storage: StorageS3, Prefix: "s3://b/p"}).ShellPrefix(); got != `env -u WALG_S3_PREFIX -u WALG_FILE_PREFIX WALG_S3_PREFIX='s3://b/p' ` {
		t.Fatalf("shell prefix = %q", got)
	}
}

func TestBackupNamesFromWALGList(t *testing.T) {
	got, err := backupNames(`[{"backup_name":"base_000000010000000000000003","time":"2026-10-05T23:25:55Z"},{"backup_name":"base_000000010000000000000006"}]`)
	if err != nil || !slices.Equal(got, []string{"base_000000010000000000000003", "base_000000010000000000000006"}) {
		t.Fatalf("names = %v, %v", got, err)
	}
	if got, err := backupNames("[]\n"); err != nil || len(got) != 0 {
		t.Fatalf("names = %v, %v", got, err)
	}
	if _, err := backupNames("ERROR: storage unreachable"); err == nil {
		t.Fatal("output that is not a JSON list was accepted")
	}
}
