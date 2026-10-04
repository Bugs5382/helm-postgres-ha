package commands

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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	golog "github.com/Bugs5382/go-log"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/Bugs5382/helm-postgres-ha/internal/backup"
	"github.com/Bugs5382/helm-postgres-ha/internal/lease"
	"github.com/Bugs5382/helm-postgres-ha/internal/secrets"
)

// installCmd copies the agent and wal-g into a shared volume, so the official
// PostgreSQL image runs them without being rebuilt.
func installCmd() *cobra.Command {
	var dest, walg string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Copy pgha and wal-g into a directory (the chart's init container)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			self, err := os.Executable()
			if err != nil {
				return err
			}
			for src, name := range map[string]string{self: "pgha", walg: "wal-g"} {
				if err := copyFile(src, filepath.Join(dest, name)); err != nil {
					return fmt.Errorf("install %s: %w", name, err)
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "installed %s\n", filepath.Join(dest, name))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dest, "dest", "/pgha/bin", "directory to copy the binaries into")
	cmd.Flags().StringVar(&walg, "walg", "/usr/local/bin/wal-g", "path of the wal-g binary to copy")
	return cmd
}

// syncFile flushes a copied binary to disk; tests replace it.
var syncFile = func(f *os.File) error { return f.Sync() }

// copyFile copies through a temporary file and a rename, and syncs before
// the rename: the init container's memory limit is charged for dirty page
// cache, and a large unsynced copy can get it OOM-killed.
func copyFile(src, dst string) error {
	in, err := os.Open(src) // #nosec G304 -- fixed binary paths
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755) // #nosec G302 G304 -- executables
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := syncFile(out); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func clientset() (kubernetes.Interface, error) {
	rc, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	rc.Timeout = 10 * time.Second
	return kubernetes.NewForConfig(rc)
}

func namespace(flag string) string {
	if flag != "" {
		return flag
	}
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err == nil {
		return string(b)
	}
	return "default"
}

func secretsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "secrets", Short: "Manage the cluster's generated credentials"}
	var spec, ns string
	ensure := &cobra.Command{
		Use:   "ensure",
		Short: "Create missing credential Secrets; never change existing ones (the chart's hook Job)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := logger()
			s, err := secrets.LoadSpec(spec)
			if err != nil {
				return err
			}
			cs, err := clientset()
			if err != nil {
				return err
			}
			n, err := secrets.Ensure(cmd.Context(), cs, namespace(ns), s, log)
			if err != nil {
				return err
			}
			log.Info("secrets ensured", golog.F("created", n), golog.F("listed", len(s.Items)))
			return nil
		},
	}
	ensure.Flags().StringVar(&spec, "spec", "/etc/pgha/secrets.json", "JSON list of Secrets to ensure")
	ensure.Flags().StringVar(&ns, "namespace", "", "namespace (default: the pod's)")
	cmd.AddCommand(ensure)
	return cmd
}

// switchoverCmd asks the primary to hand over by annotating the Lease. It is
// run with kubectl exec in any member's postgres container.
func switchoverCmd() *cobra.Command {
	var to, leaseName, ns string
	cmd := &cobra.Command{
		Use:   "switchover",
		Short: "Ask the primary to hand over to a standby (planned switchover)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if leaseName == "" {
				leaseName = os.Getenv("PGHA_LEASE")
			}
			if leaseName == "" {
				return errors.New("--lease is required outside a member")
			}
			cs, err := clientset()
			if err != nil {
				return err
			}
			st := lease.New(cs, namespace(ns), leaseName, time.Minute)
			if err := st.Annotate(cmd.Context(), map[string]string{lease.Switchover: to}); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "switchover to %s requested on lease %s\n", to, leaseName)
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "any", "member to hand over to, or any")
	cmd.Flags().StringVar(&leaseName, "lease", "", "cluster lease (default: PGHA_LEASE)")
	cmd.Flags().StringVar(&ns, "namespace", "", "namespace (default: the pod's)")
	return cmd
}

// backupCmd requests an immediate base backup.
func backupCmd() *cobra.Command {
	var leaseName, ns string
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Request a base backup now",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if leaseName == "" {
				leaseName = os.Getenv("PGHA_BACKUP_LEASE")
			}
			if leaseName == "" {
				return errors.New("--lease is required outside a member")
			}
			cs, err := clientset()
			if err != nil {
				return err
			}
			st := lease.New(cs, namespace(ns), leaseName, time.Minute)
			at := time.Now().UTC().Format(time.RFC3339)
			if err := st.Annotate(cmd.Context(), map[string]string{backup.Request: at}); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "backup requested at %s\n", at)
			return nil
		},
	}
	cmd.Flags().StringVar(&leaseName, "lease", "", "backup lease (default: PGHA_BACKUP_LEASE)")
	cmd.Flags().StringVar(&ns, "namespace", "", "namespace (default: the pod's)")
	return cmd
}
