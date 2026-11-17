package main

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

func run() int {
	configPath := flag.String("config", "config.yaml", "path to config file")
	haOptionsPath := flag.String("ha-options", "", "path to HA add-on options.json (overrides -config)")
	checkConfig := flag.Bool("check-config", false, "validate configuration and exit without network access")
	rollbackConfig := flag.Bool("rollback-config", false, "restore previous standalone YAML configuration and exit")
	flag.Parse()
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

	// Load config
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

	// Set up structured logging to stdout
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
		"cache_path", cfg.CachePath,
		"log_level", cfg.LogLevel,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	var workers sync.WaitGroup
	var failed atomic.Bool
	start := func(f func()) { workers.Add(1); go func() { defer workers.Done(); f() }() }

	// Create cache
	cache, err := NewRegisterCache(cfg.CachePath)
	if err != nil {
		slog.Error("failed to open cache database", "path", cfg.CachePath, "error", err)
		return 1
	}
	defer cache.Close()

	// Start web UI and Modbus server early — the persistent cache allows
	// serving cached responses before the inverter connection is established.
	var web *WebServer
	if cfg.Web.Listen != "" {
		web = NewWebServer(cfg, cache)
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

	// Connect to inverter
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

	// Clean stale registers from previous runs
	if deleted := cache.DeleteStale(cfg.CacheTTL()); deleted > 0 {
		slog.Info("cache: cleaned stale registers from previous run", "count", deleted, "ttl", cfg.CacheTTL())
	}

	// Start polling without a blocking initial scan.
	reader := NewReader(cfg, inverterClient, cache)

	// Start reader loop — also makes stats available to the web UI
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

	// Wait for shutdown signal
	<-ctx.Done()
	slog.Info("shutting down")
	workers.Wait()
	if failed.Load() {
		return 1
	}
	return 0
}
