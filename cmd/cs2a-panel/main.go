// cs2a-panel is the control plane web UI: SSR pages for admins and players
// that drive the cs2a-agent loopback API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cs2a/internal/panel"
	"cs2a/internal/panel/web"
	"cs2a/internal/version"
)

func main() {
	configPath := flag.String("config", "/etc/cs2a/panel.json", "path to panel.json")
	flag.Parse()

	fmt.Printf("cs2a-panel %s\n", version.Version)
	web.Version = version.Version

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := panel.LoadConfig(*configPath)
	if err != nil {
		logger.Error("config", "err", err)
		os.Exit(1)
	}

	store, err := panel.OpenStore(cfg.DBPath)
	if err != nil {
		logger.Error("store", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	// optional bootstrap-created admin (env-provided, one-shot)
	if err := ensureBootstrapAdmin(store); err != nil {
		logger.Warn("bootstrap admin", "err", err)
	}

	srv := panel.NewServer(cfg, store, panel.NewAgentClient(cfg.AgentURL, cfg.AgentToken), logger)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("panel listening", "addr", cfg.Listen, "agent", cfg.AgentURL)
		errCh <- httpSrv.ListenAndServe()
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

// ensureBootstrapAdmin creates the first admin from CS2A_ADMIN_USER /
// CS2A_ADMIN_PASSWORD env vars if no users exist yet (used by bootstrap.sh).
func ensureBootstrapAdmin(store *panel.Store) error {
	users, err := store.ListUsers()
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return nil
	}
	user := os.Getenv("CS2A_ADMIN_USER")
	pass := os.Getenv("CS2A_ADMIN_PASSWORD")
	if user == "" || pass == "" {
		return nil
	}
	hash, err := panel.HashPassword(pass)
	if err != nil {
		return err
	}
	if _, err := store.CreateUser(user, hash, "admin", ""); err != nil {
		return err
	}
	// scrub the env so the secret doesn't linger in child processes
	os.Unsetenv("CS2A_ADMIN_PASSWORD")
	return nil
}
