// Command scd-mock is a local stand-in for the SaaS Custom Domains API and
// proxy, for development and CI. Run `scd-mock healthcheck` to probe a
// running instance (used by the Docker HEALTHCHECK).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/saas-custom-domains/scd-mock/internal/mock"
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)
	cfg, err := mock.ConfigFromEnv(os.Getenv)
	if err != nil {
		logger.Fatal(err)
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(cfg.APIAddr))
	}

	srv, err := mock.New(cfg, logger)
	if err != nil {
		logger.Fatal(err)
	}
	servers := []*http.Server{
		{Addr: cfg.APIAddr, Handler: srv.APIHandler(), ReadHeaderTimeout: 10 * time.Second},
		{Addr: cfg.ProxyAddr, Handler: srv.ProxyHandler(), ReadHeaderTimeout: 10 * time.Second},
	}

	logger.Printf("scd-mock: API on %s, proxy on %s", cfg.APIAddr, cfg.ProxyAddr)
	logger.Printf("scd-mock: account %s, upstream %s -> %s:%d (tls=%t)",
		cfg.AccountUUID, cfg.UpstreamUUID, cfg.UpstreamHost, cfg.UpstreamPort, cfg.UpstreamTLS)
	if len(cfg.Domains) > 0 {
		logger.Printf("scd-mock: pre-created domains: %s", strings.Join(cfg.Domains, ", "))
	}
	if cfg.WebhookURL != "" {
		logger.Printf("scd-mock: sending webhooks to %s", strings.SplitN(cfg.WebhookURL, "?", 2)[0])
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s: %w", s.Addr, err)
			}
		}()
	}

	select {
	case err := <-errc:
		logger.Print(err)
		os.Exit(1)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdownCtx)
	}
}

// healthcheck probes /_mock/health on the local API listener.
func healthcheck(apiAddr string) int {
	host, port, err := net.SplitHostPort(apiAddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/_mock/health")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy:", resp.Status)
		return 1
	}
	return 0
}
