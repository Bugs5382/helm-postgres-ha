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
	"context"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	golog "github.com/Bugs5382/go-log"
	gootel "github.com/Bugs5382/go-otel"
	otelpg "github.com/Bugs5382/go-postgres/otel"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/Bugs5382/helm-postgres-ha/internal/agent"
	"github.com/Bugs5382/helm-postgres-ha/internal/backup"
	"github.com/Bugs5382/helm-postgres-ha/internal/config"
	"github.com/Bugs5382/helm-postgres-ha/internal/errs"
	"github.com/Bugs5382/helm-postgres-ha/internal/kube"
	"github.com/Bugs5382/helm-postgres-ha/internal/lease"
	"github.com/Bugs5382/helm-postgres-ha/internal/metrics"
	"github.com/Bugs5382/helm-postgres-ha/internal/peer"
	"github.com/Bugs5382/helm-postgres-ha/internal/pg"
	"github.com/Bugs5382/helm-postgres-ha/internal/pooler"
	"github.com/Bugs5382/helm-postgres-ha/internal/proc"
	"github.com/Bugs5382/helm-postgres-ha/internal/server"
)

// backupLeaseDuration bounds how long a crashed backup holds the slot.
const backupLeaseDuration = 5 * time.Minute

func runCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the agent: supervise PostgreSQL and keep this member in its role",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context())
		},
	}
}

// fileStamp changes whenever a file's content is replaced. Kubernetes swaps
// mounted ConfigMaps and Secrets through a symlink, which Stat follows.
func fileStamp(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return "missing"
	}
	return strconv.FormatInt(st.ModTime().UnixNano(), 10) + ":" + strconv.FormatInt(st.Size(), 10)
}

type walgRunner struct{ r *proc.Runner }

func (w walgRunner) Run(ctx context.Context, env []string, name string, args ...string) error {
	_, err := w.r.Run(ctx, env, name, args...)
	return err
}

func run(parent context.Context) error {
	var log golog.Logger = logger()
	cfg, err := config.LoadFromEnv()
	if err != nil {
		err = errs.New(errs.Config, err)
		log.Error(err, "invalid configuration", golog.F("code", errs.Config))
		return err
	}
	log = log.With(golog.F("pod", cfg.PodName), golog.F("cluster", cfg.Topology.Cluster))
	log.Info("agent starting", golog.F("version", Version), golog.F("replicas", cfg.Topology.Replicas))

	ctx, stop := signal.NotifyContext(parent, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// go-otel v1.3.2 still builds a metric exporter for an empty endpoint and
	// logs a failed upload every interval, so it is only started when an
	// endpoint is set.
	if cfg.OTLPEndpoint != "" {
		shutdownOtel, err := gootel.Init(ctx, "pgha", cfg.OTLPEndpoint)
		if err != nil {
			log.Warn("tracing disabled", golog.F("error", err.Error()))
		} else {
			defer func() { _ = shutdownOtel(context.Background()) }()
		}
	}

	runner := proc.New()
	if os.Getpid() == 1 {
		go runner.ReapLoop(ctx, time.Second)
	}

	material, err := peer.NewMaterial(filepath.Join(cfg.TLSDir, "tls.crt"), filepath.Join(cfg.TLSDir, "tls.key"), filepath.Join(cfg.TLSDir, "ca.crt"))
	if err != nil {
		err = errs.New(errs.PeerTLS, err)
		log.Error(err, "mutual TLS material is required", golog.F("code", errs.PeerTLS))
		return err
	}

	rc, err := rest.InClusterConfig()
	if err != nil {
		return errs.New(errs.Config, err)
	}
	rc.Timeout = 5 * time.Second
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return errs.New(errs.Config, err)
	}

	env := append(os.Environ(), "PGHOST="+cfg.SocketDir, "PGUSER=postgres", "PGDATABASE=postgres")
	node, err := pg.NewLocal(runner, log, pg.LocalConfig{
		DataDir:               cfg.DataDir,
		ConfigFile:            cfg.ConfigFile(),
		HBAFile:               cfg.HBAFile(),
		SocketDir:             cfg.SocketDir,
		PassFile:              cfg.PassFile(),
		SuperuserPasswordFile: cfg.SecretFile("superuser"),
		WalG:                  cfg.WalG(),
		InitdbArgs:            cfg.Bootstrap.InitdbArgs,
		Env:                   env,
	}, otelpg.WithTracing())
	if err != nil {
		return errs.New(errs.Config, err)
	}

	m := metrics.New()
	_, peerPort, err := net.SplitHostPort(cfg.PeerAddr)
	if err != nil {
		return errs.New(errs.Config, err)
	}
	opts := agent.Options{
		Config:     cfg,
		Log:        log,
		Node:       node,
		Leases:     lease.New(cs, cfg.Topology.Namespace, cfg.Lease, cfg.LeaseDuration),
		Peers:      peer.NewClient(material, cfg.Topology, peerPort, cfg.PeerTimeout),
		Kube:       kube.New(cs, cfg.Topology.Namespace),
		Metrics:    m,
		ReadSecret: pg.ReadSecret,
		LoadSpec:   pg.LoadSpec,
		FileStamp:  fileStamp,
	}
	if cfg.PgBouncerService != "" {
		opts.Poolers = pooler.New(
			pooler.NewEndpoints(cs, cfg.Topology.Namespace, cfg.PgBouncerService),
			pooler.ConsoleAdmin{
				Port:     6432,
				User:     pg.RolePgBouncer,
				Password: func() (string, error) { return pg.ReadSecret(cfg.SecretFile("pgbouncer")) },
				CAFile:   filepath.Join(cfg.TLSDir, "ca.crt"),
				Timeout:  3 * time.Second,
			},
			log,
		)
	}
	if cfg.Backup.Enabled {
		bl := lease.New(cs, cfg.Topology.Namespace, cfg.BackupLease, backupLeaseDuration)
		s, err := backup.New(cfg.Backup, cfg.PodName, cfg.DataDir, cfg.WalG(), env, bl, walgRunner{runner}, log, m, nil)
		if err != nil {
			return err
		}
		opts.Backups = s
	}
	a := agent.New(opts)

	health, err := server.Listen("health", cfg.HTTPAddr, server.HealthHandler(a, m.Handler()), log)
	if err != nil {
		return errs.New(errs.Config, err)
	}
	peers, err := server.ListenTLS("peer", cfg.PeerAddr, server.PeerHandler(a), peer.ServerTLS(material, cfg.Topology), log)
	if err != nil {
		return errs.New(errs.Config, err)
	}
	go health.Serve()
	go peers.Serve()

	err = a.Run(ctx)
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peers.Shutdown(sctx)
	health.Shutdown(sctx)
	log.Info("agent stopped")
	return err
}
