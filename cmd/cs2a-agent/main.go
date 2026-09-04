// cs2a-agent runs next to the CS2 dedicated server: it executes panel
// requests over a loopback HTTP API (service control, RCON, config, plugins).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cs2a/internal/agent"
	"cs2a/internal/version"
)

func main() {
	configPath := flag.String("config", "/etc/cs2a/agent.json", "path to agent.json")
	flag.Parse()

	fmt.Printf("cs2a-agent %s\n", version.Version)

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := agent.LoadConfig(*configPath)
	if err != nil {
		logger.Error("config", "err", err)
		os.Exit(1)
	}

	store, err := agent.OpenStore(cfg.DBPath)
	if err != nil {
		logger.Error("store", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	if err := os.MkdirAll(cfg.PluginCache, 0o755); err != nil {
		logger.Error("plugin cache", "err", err)
		os.Exit(1)
	}

	srv := agent.NewServer(cfg, store)
	wh := agent.NewWhitelist(cfg)
	gh := agent.NewGHClient(os.Getenv("GITHUB_TOKEN"))
	inst := agent.NewInstaller(cfg, store, agent.DefaultCatalog(), gh)
	loadouts := agent.NewLoadoutStore(cfg, store)
	defer loadouts.Close()

	api := agent.NewAPI(cfg, srv, wh, inst, loadouts)

	listen := cfg.Listen
	if listen == "" {
		listen = agent.DefaultListen
	}
	httpSrv := &http.Server{
		Addr:              listen,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		logger.Error("listen", "addr", listen, "err", err)
		os.Exit(1)
	}
	// loopback binding is the security boundary for MVP: refuse to serve on
	// a public interface unless explicitly overridden.
	if host, _, err := net.SplitHostPort(listen); err == nil && host != "" && host != "127.0.0.1" && host != "localhost" && host != "::1" {
		if os.Getenv("CS2A_AGENT_EXPOSE") != "1" {
			logger.Error("agent refused to bind non-loopback address without CS2A_AGENT_EXPOSE=1", "addr", listen)
			os.Exit(1)
		}
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("agent listening", "addr", listen, "cs2_dir", cfg.CS2Dir)
		errCh <- httpSrv.Serve(ln)
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-stop:
		logger.Info("shutting down", "signal", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("serve", "err", err)
			os.Exit(1)
		}
	}

}
