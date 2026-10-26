// mea-stream-gateway runs once on each GPU host. See stream/README.md for
// configuration and stream/deploy for the systemd unit.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/gateway"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/internal/env"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/token"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	pub, err := token.ParsePublicKey(env.Secret("MEA_STREAM_PUBLIC_KEY"))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sampler := &gateway.Sampler{}
	go sampler.Run(ctx)

	g, err := gateway.New(gateway.Config{
		HostID:         env.String("MEA_HOST_ID", ""),
		PublicKey:      pub,
		MediaPort:      env.Int("MEA_MEDIA_PORT", 47998),
		PublicIPs:      env.List("MEA_PUBLIC_IPS"),
		ICETCP:         env.Bool("MEA_ICE_TCP", true),
		AllowedOrigins: env.List("MEA_ALLOWED_ORIGINS"),
		AdminToken:     env.Secret("MEA_ADMIN_TOKEN"),
		Registry:       &gateway.FileRegistry{Path: env.String("MEA_REGISTRY_FILE", "/var/lib/type3/stream-agents.json")},
		Limits: gateway.Limits{
			// Until the L4 density benchmark passes, one stream per host.
			MaxSessions:   env.Int("MEA_MAX_SESSIONS", 1),
			MaxEncoderPct: env.Float("MEA_MAX_ENCODER_PCT", 85),
			MaxGPUPct:     env.Float("MEA_MAX_GPU_PCT", 95),
			MinFreeMemMiB: env.Float("MEA_MIN_FREE_MEM_MIB", 1024),
		},
		Sampler: sampler,
		Log:     log,
	})
	if err != nil {
		return err
	}
	defer g.Close()

	public := &http.Server{
		Addr:              env.String("MEA_LISTEN", ":443"),
		Handler:           g.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	admin := &http.Server{
		Addr:              env.String("MEA_ADMIN_LISTEN", "127.0.0.1:47990"),
		Handler:           g.AdminHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errc := make(chan error, 2)
	go func() { errc <- admin.ListenAndServe() }()
	go func() {
		switch {
		case env.String("MEA_TLS_DOMAIN", "") != "":
			// TLS-ALPN-01 on :443, no extra port or proxy needed.
			m := &autocert.Manager{
				Prompt:     autocert.AcceptTOS,
				HostPolicy: autocert.HostWhitelist(env.String("MEA_TLS_DOMAIN", "")),
				Cache:      autocert.DirCache(env.String("MEA_TLS_CACHE", "/var/lib/mea-stream/acme")),
				Email:      env.String("MEA_TLS_EMAIL", ""),
			}
			public.TLSConfig = &tls.Config{GetCertificate: m.GetCertificate, NextProtos: []string{"h2", "http/1.1", "acme-tls/1"}, MinVersion: tls.VersionTLS12}
			errc <- public.ListenAndServeTLS("", "")
		case env.String("MEA_TLS_CERT", "") != "":
			errc <- public.ListenAndServeTLS(env.String("MEA_TLS_CERT", ""), env.String("MEA_TLS_KEY", ""))
		default:
			log.Warn("serving signalling without TLS; use only behind a TLS terminator or for local testing")
			errc <- public.ListenAndServe()
		}
	}()
	log.Info("gateway up", "host", env.String("MEA_HOST_ID", ""), "media", g.MediaAddr().String(), "signal", public.Addr, "admin", admin.Addr)
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	public.Shutdown(sctx)
	admin.Shutdown(sctx)
	return nil
}
