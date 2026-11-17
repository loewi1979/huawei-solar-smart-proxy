package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Inverter            InverterConfig  `yaml:"inverter"`
	Server              ServerConfig    `yaml:"server"`
	Web                 WebConfig       `yaml:"web"`
	Polling             PollingConfig   `yaml:"polling"`
	ForwardUnknownReads bool            `yaml:"forward_unknown_reads"`
	RegisterGroups      []RegisterGroup `yaml:"register_groups"`
	LogLevel            string          `yaml:"log_level"`
	CachePath           string          `yaml:"cache_path"`
	CacheTTLH           int             `yaml:"cache_ttl_h"`
	ReadOnly            bool            `yaml:"read_only"`
	AllowWrites         *bool           `yaml:"allow_writes,omitempty"` // legacy alias, normalized by ParseConfig
	MQTT                MQTTConfig      `yaml:"mqtt"`
}

type InverterConfig struct {
	WaitAfterConnectMs int    `yaml:"wait_after_connect_ms"`
	Host               string `yaml:"host"`
	Port               int    `yaml:"port"`
	UnitIDs            []byte `yaml:"unit_ids"`
	TimeoutMs          int    `yaml:"timeout_ms"`
}

type ServerConfig struct {
	Listen string `yaml:"listen"`
}

type WebConfig struct {
	Listen string `yaml:"listen"`
	Token  string `yaml:"token"`
}

type PollingConfig struct {
	FastIntervalS int `yaml:"fast_interval_s"`
	FastMaxAgeS   int `yaml:"fast_max_age_s"`
	SlowMaxAgeS   int `yaml:"slow_max_age_s"`
	ReadPauseMs   int `yaml:"read_pause_ms"`
	SlowIntervalS int `yaml:"slow_interval_s"`
}

type RegisterGroup struct {
	Priority int    `yaml:"priority"`
	Name     string `yaml:"name"`
	Address  uint16 `yaml:"address"`
	Count    uint16 `yaml:"count"`
	Poll     string `yaml:"poll"` // "fast" or "slow"
}

func (c *Config) ReadPause() time.Duration {
	return time.Duration(c.Polling.ReadPauseMs) * time.Millisecond
}

func (c *Config) SlowInterval() time.Duration {
	return time.Duration(c.Polling.SlowIntervalS) * time.Second
}

func (c *Config) InverterTimeout() time.Duration {
	return time.Duration(c.Inverter.TimeoutMs) * time.Millisecond
}

func (c *Config) CacheTTL() time.Duration {
	return time.Duration(c.CacheTTLH) * time.Hour
}

func (c *Config) InverterAddr() string {
	return net.JoinHostPort(c.Inverter.Host, fmt.Sprint(c.Inverter.Port))
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	return ParseConfig(data)
}

func ParseConfig(data []byte) (*Config, error) {

	cfg := &Config{
		Inverter: InverterConfig{
			WaitAfterConnectMs: 1000,
			Port:               502,
			UnitIDs:            []byte{1},
			TimeoutMs:          5000,
		},
		Server: ServerConfig{
			Listen: "127.0.0.1:1502",
		},
		Web: WebConfig{
			Listen: "127.0.0.1:8080",
		},
		Polling: PollingConfig{
			FastIntervalS: 5,
			FastMaxAgeS:   30,
			SlowMaxAgeS:   900,
			ReadPauseMs:   500,
			SlowIntervalS: 300,
		},
		ForwardUnknownReads: false,
		ReadOnly:            true,
		MQTT:                defaultMQTTConfig(),
		LogLevel:            "info",
		CachePath:           "cache.db",
		CacheTTLH:           2,
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("exactly one YAML document required")
	}
	// Detect explicit keys, including nulls. Never let two opposite settings
	// silently override each other during restore or migration.
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	var hasReadOnly, hasAllowWrites bool
	if len(document.Content) > 0 {
		fields := document.Content[0].Content
		for i := 0; i+1 < len(fields); i += 2 {
			if fields[i].Value == "read_only" || fields[i].Value == "allow_writes" {
				if fields[i+1].Tag != "!!bool" {
					return nil, fmt.Errorf("%s must be true or false", fields[i].Value)
				}
				if fields[i].Value == "read_only" {
					hasReadOnly = true
				} else {
					hasAllowWrites = true
				}
			}
		}
	}
	var effectivePolicy struct {
		ReadOnly    *bool `yaml:"read_only"`
		AllowWrites *bool `yaml:"allow_writes"`
	}
	if err := document.Decode(&effectivePolicy); err != nil {
		return nil, err
	}
	if hasReadOnly && hasAllowWrites || effectivePolicy.ReadOnly != nil && effectivePolicy.AllowWrites != nil {
		return nil, fmt.Errorf("use read_only or legacy allow_writes, not both")
	}
	if cfg.AllowWrites != nil {
		cfg.ReadOnly = !*cfg.AllowWrites
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (cfg *Config) Validate() error {
	bad := func(s string) error { return fmt.Errorf("invalid configuration: %s", s) }
	if cfg.Polling.FastIntervalS > 86400 || cfg.Polling.SlowIntervalS > 86400 || cfg.Polling.FastMaxAgeS > 604800 || cfg.Polling.SlowMaxAgeS > 604800 || cfg.CacheTTLH > 8760 {
		return bad("polling/freshness/retention exceeds supported limits")
	}
	if cfg.LogLevel != "debug" && cfg.LogLevel != "info" && cfg.LogLevel != "warn" && cfg.LogLevel != "error" {
		return bad("log_level")
	}
	if cfg.Web.Listen != "" {
		host, _, err := net.SplitHostPort(cfg.Web.Listen)
		if err == nil && host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback()) && len(cfg.Web.Token) < 24 {
			return bad("non-loopback web UI requires token of at least 24 characters")
		}
	}
	if cfg.Inverter.Port < 1 || cfg.Inverter.Port > 65535 || cfg.Inverter.TimeoutMs < 100 || cfg.Inverter.TimeoutMs > 60000 || cfg.Inverter.WaitAfterConnectMs < 0 || cfg.Inverter.WaitAfterConnectMs > 10000 {
		return bad("inverter port/timeout")
	}
	if cfg.Polling.ReadPauseMs < 100 || cfg.Polling.ReadPauseMs > 60000 || cfg.Polling.FastIntervalS < 1 || cfg.Polling.SlowIntervalS < 1 || cfg.Polling.FastMaxAgeS < cfg.Polling.FastIntervalS || cfg.Polling.SlowMaxAgeS < cfg.Polling.SlowIntervalS || cfg.CacheTTLH < 1 {
		return bad("polling intervals, freshness or retention")
	}
	for _, addr := range []string{cfg.Server.Listen, cfg.Web.Listen} {
		if addr != "" {
			if _, _, err := net.SplitHostPort(addr); err != nil {
				return bad("listen address")
			}
		}
	}
	if cfg.Server.Listen == "" || cfg.CachePath == "" {
		return bad("server.listen and cache_path required")
	}
	if err := cfg.MQTT.Validate(cfg); err != nil {
		return err
	}

	if cfg.Inverter.Host == "" {
		return fmt.Errorf("inverter.host is required")
	}

	if len(cfg.RegisterGroups) == 0 {
		return fmt.Errorf("at least one register_group is required")
	}

	if len(cfg.Inverter.UnitIDs) == 0 {
		return fmt.Errorf("inverter.unit_ids is required (e.g. [1])")
	}

	// Validate no duplicate unit IDs
	seen := make(map[byte]bool, len(cfg.Inverter.UnitIDs))
	for _, uid := range cfg.Inverter.UnitIDs {
		if uid < 1 || uid > 247 {
			return bad("unit ID outside 1..247")
		}
		if seen[uid] {
			return fmt.Errorf("inverter.unit_ids: duplicate unit ID %d", uid)
		}
		seen[uid] = true
	}

	for i, g := range cfg.RegisterGroups {
		if g.Name == "" {
			return bad("register group name required")
		}
		if uint32(g.Address)+uint32(g.Count) > 65536 {
			return bad("register range overflow")
		}
		for _, other := range cfg.RegisterGroups[:i] {
			if g.Name == other.Name || uint32(g.Address) < uint32(other.Address)+uint32(other.Count) && uint32(other.Address) < uint32(g.Address)+uint32(g.Count) {
				return bad("duplicate names or overlapping register groups")
			}
		}
		if g.Count == 0 {
			return fmt.Errorf("register_groups[%d] (%s): count must be > 0", i, g.Name)
		}
		if g.Count > 125 {
			return fmt.Errorf("register_groups[%d] (%s): count must be <= 125 (modbus limit)", i, g.Name)
		}
		if g.Poll != "fast" && g.Poll != "slow" {
			return fmt.Errorf("register_groups[%d] (%s): poll must be 'fast' or 'slow'", i, g.Name)
		}
	}

	return nil
}
