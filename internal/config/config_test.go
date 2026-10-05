package config

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
	"strings"
	"testing"
	"time"
)

func env(over map[string]string) Getenv {
	base := map[string]string{
		"PGHA_CLUSTER":          "pg",
		"POD_NAMESPACE":         "db",
		"PGHA_HEADLESS_SERVICE": "pg-headless",
		"POD_NAME":              "pg-1",
	}
	for k, v := range over {
		base[k] = v
	}
	return func(k string) string { return base[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Topology.Replicas != 3 || c.Lease != "pg" || c.BackupLease != "pg-backup" {
		t.Fatalf("defaults wrong: %+v", c)
	}
	if c.LeaseDuration != 15*time.Second || c.RenewDeadline != 10*time.Second {
		t.Fatalf("lease timing wrong: %s %s", c.LeaseDuration, c.RenewDeadline)
	}
	if c.SecretFile("replication") != "/etc/pgha/secrets/replication/password" {
		t.Fatalf("secret file = %s", c.SecretFile("replication"))
	}
	if len(c.Bootstrap.InitdbArgs) != 3 {
		t.Fatalf("initdb args = %v", c.Bootstrap.InitdbArgs)
	}
}

func TestLoadRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"missing cluster":  {map[string]string{"PGHA_CLUSTER": ""}, "PGHA_CLUSTER is required"},
		"not a member":     {map[string]string{"POD_NAME": "pg-5"}, "not a member"},
		"bad duration":     {map[string]string{"PGHA_LEASE_DURATION": "soon"}, "not a duration"},
		"deadline too big": {map[string]string{"PGHA_RENEW_DEADLINE": "14s"}, "must be less than the lease duration"},
		"bad mode":         {map[string]string{"PGHA_BOOTSTRAP_MODE": "clone"}, "unknown bootstrap mode"},
		"restore no store": {map[string]string{"PGHA_BOOTSTRAP_MODE": "restore"}, "needs PGHA_RESTORE_PREFIX"},
		"two targets": {map[string]string{
			"PGHA_BOOTSTRAP_MODE": "restore", "PGHA_RESTORE_PREFIX": "s3://b/p",
			"PGHA_RESTORE_TARGET_TIME": "2026-01-01 00:00:00+00", "PGHA_RESTORE_TARGET_LSN": "0/1",
		}, "at most one restore target"},
		"bad source":  {map[string]string{"PGHA_BACKUP_ENABLED": "true", "PGHA_BACKUP_SOURCE": "any"}, "unknown backup source"},
		"bad bool":    {map[string]string{"PGHA_BACKUP_ENABLED": "yes please"}, "not a boolean"},
		"bad lag":     {map[string]string{"PGHA_MAX_LAG_ON_FAILOVER": "-1"}, "non-negative"},
		"slow peers":  {map[string]string{"PGHA_PEER_TIMEOUT": "6s"}, "peer timeout"},
		"no replicas": {map[string]string{"PGHA_REPLICAS": "0"}, "replicas must be at least 1"},
		"bad sync":    {map[string]string{"PGHA_SYNC_MODE": "always"}, "unknown sync mode"},
		"zero sync":   {map[string]string{"PGHA_SYNC_COUNT": "0"}, "PGHA_SYNC_COUNT"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(env(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestLoadFileBackupPath(t *testing.T) {
	c, err := Load(env(map[string]string{"PGHA_BACKUP_ENABLED": "true", "PGHA_BACKUP_PATH": "/backup/pg"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Backup.Path != "/backup/pg" {
		t.Fatalf("backup path = %q", c.Backup.Path)
	}
}
