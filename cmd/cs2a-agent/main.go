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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"cs2a/internal/agent"
	"cs2a/internal/version"
)

func main() {
	configPath := flag.String("config", "/etc/cs2a/agent.json", "path to agent.json")
	check := flag.Bool("check", false, "validate the config file and exit")
	flag.Parse()

	// -check is the installer's config gate: the agent itself is the only
	// thing that knows what a valid agent.json is, so bootstrap no longer has
	// to shell out to python3 (and no longer skips the check when it is
	// missing).
	if *check {
		cfg, err := agent.LoadConfig(*configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cs2a-agent: %v\n", err)
			os.Exit(1)
		}
		// The install root is only checked here, never in LoadConfig: a running
		// agent must still start with a broken path so the panel can explain
		// the problem instead of the service simply being dead.
		if _, err := os.Stat(filepath.Join(cfg.CSGODir(), "gameinfo.gi")); err != nil {
			fmt.Fprintf(os.Stderr, "cs2a-agent: cs2_dir %s does not contain game/csgo/gameinfo.gi\n", cfg.CS2Dir)
			os.Exit(1)
		}
		fmt.Printf("config ok: cs2_dir=%s service=%s rcon=%s\n", cfg.CS2Dir, cfg.ServiceName, cfg.RCONAddr)
		return
	}

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

	// The game unit's EnvironmentFile must exist before the unit can start;
	// systemd fails a unit whose EnvironmentFile is missing. bootstrap creates
	// it, but the agent re-ensures so a hand-deleted file cannot wedge the
	// next server start. Best effort: an agent that cannot write it should
	// still come up to serve status.
	if cfg.MapEnvFile != "" {
		if err := agent.EnsureMapEnv(cfg.MapEnvFile); err != nil {
			logger.Error("map env file", "err", err)
		}
	}

	srv := agent.NewServer(cfg, store)
	wh := agent.NewWhitelist(cfg)
	gh := agent.NewGHClient(os.Getenv("GITHUB_TOKEN"))
	inst := agent.NewInstaller(cfg, store, agent.DefaultCatalog(), gh)
	loadouts := agent.NewLoadoutStore(cfg, store)
	defer loadouts.Close()

	// The updater needs the live player count to honour "never kick anyone
	// for an update": Status answers with the RCON view when the server is
	// up, and zero when it is not (which is also a fine time to update).
	updater := agent.NewUpdater(cfg, agent.NewSystemd(cfg.ServiceName), func() int {
		st := srv.Status(context.Background())
		if st.Rcon == nil {
			return 0
		}
		return st.Rcon.Humans
	})
	api := agent.NewAPI(cfg, srv, wh, inst, loadouts).WithUpdater(updater)

	// Background CS2 update check: every few hours, compare the installed
	// build with Steam's and (with auto_update) apply it when the server is
	// empty. Dies with the shutdown ctx below.
	loopCtx, stopLoop := context.WithCancel(context.Background())
	defer stopLoop()
	go updater.RunAutoLoop(loopCtx)

	// Bootstrap's recommended plugin stack, if the operator chose it: the
	// agent queues the installs as jobs the panel can watch, and clears the
	// key once they are done. A failed install keeps its id pending, so the
	// next boot retries it (a Valve update day that broke an upstream release
	// is exactly when this matters). A boot without a pending list — the
	// common case from the second boot on — does nothing at all, and in
	// particular does not rewrite the config file.
	if len(cfg.PendingPlugins) > 0 {
		known := api.InstallPending(cfg.PendingPlugins, func(failed []string) {
			// Queue drained: drop the installed ids from the config so a
			// later boot does not reinstall them; keep the failed ones
			// pending so the next boot retries exactly those.
			if len(failed) == len(cfg.PendingPlugins) && len(failed) > 0 {
				// nothing succeeded — leave the file untouched
				return
			}
			cfg.PendingPlugins = failed
			if err := cfg.Persist(); err != nil {
				logger.Error("update pending plugins", "err", err)
			}
		})
		if len(known) > 0 {
			logger.Info("installing bootstrap plugin stack", "plugins", strings.Join(known, ", "))
		}
	}

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
		stopLoop()
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
