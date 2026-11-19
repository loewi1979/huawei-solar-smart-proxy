package main

// The entry point handles configuration commands and ties the proxy services
// to a shared lifetime for startup and shutdown.

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
)

func main() {
	os.Exit(run())
}

// run returns the process exit code after its deferred cleanup has run.
func run() int {
	configPath := flag.String("config", "config.yaml", "path to config file")
	haOptionsPath := flag.String("ha-options", "", "path to HA add-on options.json (overrides -config)")
	checkConfig := flag.Bool("check-config", false, "validate configuration and exit without network access")
	rollbackConfig := flag.Bool("rollback-config", false, "restore previous standalone YAML configuration and exit")
	flag.Parse()
	// Configuration-only commands finish before starting network services.
	if *rollbackConfig {
		if *haOptionsPath != "" {
			slog.Error("rollback requires standalone YAML")
			return 1
		}
		if err := (&ConfigStore{Path: *configPath}).Rollback(); err != nil {
			slog.Error("rollback failed", "error", err)
			return 1
		}
		return 0
	}

	// Add-on options take precedence when both paths are supplied.
	var cfg *Config
	var err error
	if *haOptionsPath != "" {
		cfg, err = LoadHAOptions(*haOptionsPath)
	} else {
		cfg, err = LoadConfig(*configPath)
	}
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}
	if *checkConfig {
		slog.Info("configuration valid")
		return 0
	}

	var logLevel slog.Level
	switch cfg.LogLevel {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))
	slog.SetDefault(logger)

	slog.Info("config loaded",
		"inverter", cfg.InverterAddr(),
		"server_listen", cfg.Server.Listen,
		"web_listen", cfg.Web.Listen,
		"register_groups", len(cfg.RegisterGroups),
		"read_pause_ms", cfg.Polling.ReadPauseMs,
		"slow_interval_s", cfg.Polling.SlowIntervalS,
		"forward_unknown_reads", cfg.ForwardUnknownReads,
		"read_only", cfg.ReadOnly,
		"wait_after_connect_ms", cfg.Inverter.WaitAfterConnectMs,
		"cache_path", cfg.CachePath,
		"log_level", cfg.LogLevel,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	// A service failure or process signal cancels the shared context.
	// Wait for the workers before closing their shared resources.
	var workers sync.WaitGroup
	var failed atomic.Bool
	start := func(f func()) { workers.Add(1); go func() { defer workers.Done(); f() }() }

	cache, err := NewRegisterCache(cfg.CachePath)
	if err != nil {
		slog.Error("failed to open cache database", "path", cfg.CachePath, "error", err)
		return 1
	}
	defer cache.Close()

	// Start listeners early so operators can inspect status and clients can
	// use fresh persisted values while the inverter is unavailable.
	var web *WebServer
	if cfg.Web.Listen != "" {
		web = NewWebServer(cfg, cache)
		// YAML backup and rollback need a standalone file to manage.
		if *haOptionsPath == "" {
			web.store = &ConfigStore{Path: *configPath}
		}
		start(func() {
			if err := web.ListenAndServe(ctx); err != nil {
				failed.Store(true)
				slog.Error("web server error", "error", err)
				cancel()
			}
		})
	}

	server := NewServer(cfg, cache)
	start(func() {
		if err := server.ListenAndServe(ctx); err != nil {
			failed.Store(true)
			slog.Error("server error", "error", err)
			cancel()
		}
	})

	// Attach the lazy upstream client. A poll or forwarded request will open
	// the first connection.
	slog.Info("connecting to inverter", "address", cfg.InverterAddr(), "unit_ids", cfg.Inverter.UnitIDs)
	inverterClient, err := NewInverterClient(cfg)
	if err != nil {
		slog.Error("failed to connect to inverter", "error", err)
		cancel()
		workers.Wait()
		return 1
	}
	defer inverterClient.Close()
	slog.Info("inverter client ready; connection will be established on first poll")
	server.SetInverterClient(inverterClient)

	// Evict rows beyond disk retention. Reads enforce their own age limits.
	if deleted := cache.DeleteStale(cfg.CacheTTL()); deleted > 0 {
		slog.Info("cache: cleaned stale registers from previous run", "count", deleted, "ttl", cfg.CacheTTL())
	}

	// Let the due-job scheduler populate the cache in the background.
	reader := NewReader(cfg, inverterClient, cache)

	if web != nil {
		web.SetReader(reader)
	}
	start(func() { reader.Run(ctx) })
	if cfg.MQTT.Enabled {
		if cfg.MQTT.Embedded {
			broker, e := startBroker(cfg.MQTT)
			if e != nil {
				failed.Store(true)
				slog.Error("MQTT broker failed", "error", e)
				cancel()
			} else {
				start(func() { runMQTT(ctx, cache, cfg.MQTT, broker); broker.Close() })
			}
		} else {
			start(func() { runMQTT(ctx, cache, cfg.MQTT, nil) })
		}
	}

	<-ctx.Done()
	slog.Info("shutting down")
	workers.Wait()
	if failed.Load() {
		return 1
	}
	return 0
}
