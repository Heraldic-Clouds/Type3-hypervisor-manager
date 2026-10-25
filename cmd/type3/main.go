// Command type3 runs the Type3 LXD management API for Mise En Abyme.
//
//	type3 [serve]        start the API server (default)
//	type3 gen-key NAME ROLE   print a new API key and its config entry
//	type3 hash-key       read a key from stdin and print its SHA-256
//	type3 check-config   validate configuration and exit
//	type3 version        print the version
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/audit"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/auth"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/config"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/containers"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/hosts"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/httpapi"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/images"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/logging"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/lxdclient"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/metrics"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/model"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/operations"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/profiles"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/secrets"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/store"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/version"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "gen-key":
		err = genKey(os.Args[2:])
	case "hash-key":
		err = hashKey(os.Stdin)
	case "check-config":
		err = checkConfig()
	case "version", "--version", "-v":
		fmt.Printf("type3 %s (%s)\n", version.Version, version.Commit)
	case "help", "--help", "-h":
		fmt.Println("usage: type3 [serve|gen-key NAME ROLE|hash-key|check-config|version]")
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "type3:", err)
		os.Exit(1)
	}
}

func genKey(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: type3 gen-key NAME ROLE (role: viewer, operator or admin)")
	}
	if _, err := auth.ParseRole(args[1]); err != nil {
		return err
	}
	key, err := auth.GenerateKey()
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "API key (shown once, store it in your secret manager):")
	fmt.Println(key)
	fmt.Fprintln(os.Stderr, "\nAdd this entry to TYPE3_API_KEYS (comma separated) or the keys file:")
	fmt.Fprintf(os.Stderr, "%s:%s:%s\n", args[0], strings.ToLower(args[1]), auth.HashKey(key))
	return nil
}

func hashKey(r io.Reader) error {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	key := strings.TrimSpace(line)
	if key == "" {
		return errors.New("no key on stdin")
	}
	fmt.Println(auth.HashKey(key))
	return nil
}

func loadAuth(cfg *config.Config) (*auth.Authenticator, error) {
	keys, err := auth.LoadKeyFile(cfg.APIKeysFile)
	if err != nil {
		return nil, err
	}
	envKeys, err := auth.ParseKeySpecs(cfg.APIKeys)
	if err != nil {
		return nil, err
	}
	a, err := auth.New(append(keys, envKeys...))
	if err != nil {
		return nil, err
	}
	if a.Count() == 0 {
		return nil, errors.New("no API keys configured; create one with `type3 gen-key NAME admin` and set TYPE3_API_KEYS or TYPE3_API_KEYS_FILE")
	}
	return a, nil
}

func checkConfig() error {
	cfg, err := config.Load(".env")
	if err != nil {
		return err
	}
	if _, err := loadAuth(cfg); err != nil {
		return err
	}
	if _, err := profiles.LoadCatalog(cfg.ProfilesFile); err != nil {
		return err
	}
	fmt.Println("configuration OK")
	return nil
}

func serve() error {
	cfg, err := config.Load(".env")
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	log := logging.New(os.Stderr, cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(log)

	authn, err := loadAuth(cfg)
	if err != nil {
		return err
	}
	auditLog, err := audit.Open(cfg.AuditLogFile, os.Stderr)
	if err != nil {
		return fmt.Errorf("open audit log: %w", err)
	}
	defer auditLog.Close()

	catalog, err := profiles.LoadCatalog(cfg.ProfilesFile)
	if err != nil {
		return err
	}
	st, err := store.OpenFileStore(cfg.StateFile)
	if err != nil {
		return err
	}
	defer st.Close()

	resolver := &secrets.Resolver{}
	if s3f, err := secrets.NewS3Fetcher(context.Background(), cfg.AWSRegion); err == nil {
		resolver.S3 = s3f
	} else {
		log.Warn("s3:// secret references disabled", "err", err)
	}

	reg := hosts.NewRegistry(st, lxdclient.TLSConnector{}, resolver, hosts.Options{
		ClientCertRef: cfg.LXDClientCertRef, ClientKeyRef: cfg.LXDClientKeyRef, CACertRef: cfg.LXDCACertRef,
		RequestTimeout: cfg.LXDRequestTimeout, FailureThreshold: cfg.HostFailureThreshold,
		AllowPublicEndpoints: cfg.AllowPublicEndpoints, AllowUnixSocket: cfg.AllowUnixSocket,
		DefaultNetwork: cfg.DefaultNetwork, DefaultStoragePool: cfg.DefaultStoragePool,
		CPUOvercommit: cfg.CPUOvercommit, MemoryOvercommit: cfg.MemoryOvercommit,
		MaxContainersPerGPU: cfg.MaxContainersPerGPU, UserAgent: version.UserAgent(),
	}, log)
	defer reg.Close()

	met := metrics.New()
	ops := operations.NewManager(st, operations.Options{
		DefaultTimeout: cfg.OperationTimeout, Retention: cfg.OperationRetention,
		OnFinish: func(op model.Operation) {
			met.Inc("type3_operations_finished_total", "Finished operations by type and status.",
				map[string]string{"type": op.Type, "status": string(op.Status)})
			auditLog.Record(context.Background(), audit.Event{
				Actor: op.Actor, Action: op.Type, ResourceType: op.ResourceType, ResourceID: op.ResourceID,
				Outcome: string(op.Status), OperationID: op.ID, Error: op.ErrorCode,
			})
		},
	}, log)
	interrupted, err := ops.Recover(context.Background())
	if err != nil {
		return fmt.Errorf("recover operations: %w", err)
	}
	if len(interrupted) > 0 {
		log.Warn("operations interrupted by previous shutdown were marked failed", "count", len(interrupted))
	}

	im := images.NewManager(reg, ops, images.SimpleStreamsLister{UserAgent: version.UserAgent()}, images.Options{
		Servers: cfg.ImageServers, DefaultServer: cfg.DefaultImageServer, Timeout: cfg.OperationTimeout,
	}, log)
	cm := containers.NewManager(st, reg, im, ops, catalog, containers.Options{
		GracefulStopTimeout: cfg.GracefulStopTimeout, OperationTimeout: cfg.OperationTimeout,
		IdmapIsolated: cfg.IdmapIsolated, PortIsolation: cfg.NICPortIsolation,
	}, log)

	bg, stopBG := context.WithCancel(context.Background())
	defer stopBG()
	reg.OnReconnect(func(id string) {
		if _, err := cm.Reconcile(bg, id); err != nil {
			log.Warn("reconcile after reconnect", "host", id, "err", err)
		}
	})
	registerCollectors(met, st, ops)

	handler := httpapi.New(httpapi.Deps{
		Hosts: reg, Containers: cm, Images: im, Ops: ops, Catalog: catalog, Auth: authn,
		Audit: auditLog, Metrics: met, Store: st, Log: log, MaxBodyBytes: cfg.MaxRequestBodyBytes,
	})
	srv := &http.Server{
		Addr: cfg.ListenAddr, Handler: handler,
		ReadHeaderTimeout: cfg.HTTPReadHeaderTimeout, ReadTimeout: 30 * time.Second,
		WriteTimeout: 2 * time.Minute, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	if cfg.TLSCertFile != "" {
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	} else if !cfg.IsLoopback() {
		log.Warn("serving plain HTTP on a non-loopback address; put Type3 behind TLS or set TYPE3_TLS_CERT_FILE/TYPE3_TLS_KEY_FILE",
			"listen", cfg.ListenAddr)
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); reg.Run(bg, cfg.HealthInterval) }()
	go func() {
		defer wg.Done()
		// Give the first health pass a moment, then repair state left by any
		// interrupted operations before starting the periodic loop.
		select {
		case <-bg.Done():
			return
		case <-time.After(2 * time.Second):
		}
		cm.ReconcileAll(bg)
		cm.Run(bg, cfg.ReconcileInterval)
	}()
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-bg.Done():
				return
			case <-t.C:
				ops.Prune(bg)
			}
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		log.Info("type3 listening", "addr", cfg.ListenAddr, "tls", cfg.TLSCertFile != "", "version", version.Version,
			"key_count", authn.Count(), "tiers", catalog.Names())
		var err error
		if cfg.TLSCertFile != "" {
			err = srv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			err = srv.ListenAndServe()
		}
		if !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-sigCtx.Done():
		log.Info("shutdown requested")
	case err := <-errCh:
		if err != nil {
			stopBG()
			return fmt.Errorf("http server: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	if err := ops.Shutdown(shutdownCtx); err != nil {
		log.Warn("operations still running at shutdown were cancelled", "err", err)
	}
	stopBG()
	wg.Wait()
	log.Info("type3 stopped")
	return nil
}

func registerCollectors(met *metrics.Registry, st store.Store, ops *operations.Manager) {
	met.Collect(func(emit func(string, string, map[string]string, float64)) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if hs, err := st.ListHosts(ctx); err == nil {
			counts := map[model.HostStatus]int{}
			for _, h := range hs {
				counts[h.Status]++
				emit("type3_host_nvidia_gpus", "NVIDIA GPUs usable for containers per host.", map[string]string{"host": h.ID}, float64(h.Capacity.NvidiaGPUs))
			}
			for _, s := range []model.HostStatus{model.HostOnline, model.HostDegraded, model.HostOffline, model.HostUnknown} {
				emit("type3_hosts", "Registered hosts by status.", map[string]string{"status": string(s)}, float64(counts[s]))
			}
		}
		if cs, err := st.ListContainers(ctx, store.ContainerFilter{}); err == nil {
			type k struct{ status, tier string }
			counts := map[k]int{}
			for _, c := range cs {
				counts[k{string(c.Status), c.Tier}]++
			}
			for key, n := range counts {
				emit("type3_containers", "Managed containers by status and tier.", map[string]string{"status": key.status, "tier": key.tier}, float64(n))
			}
		}
		for s, n := range ops.Counts() {
			emit("type3_operations", "Tracked operations by status.", map[string]string{"status": string(s)}, float64(n))
		}
	})
}
