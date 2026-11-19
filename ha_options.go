package main

// HA add-on options are translated into the same Config used in standalone
// mode. The add-on uses a fixed cache location and built-in register groups.

import (
	"encoding/json"
	"fmt"
	"os"
)

// HAOptions mirrors the schema in config.json (HA add-on options).
type HAOptions struct {
	InverterHost        string `json:"inverter_host"`
	InverterPort        int    `json:"inverter_port"`
	UnitIDs             []int  `json:"unit_ids"`
	ReadPauseMs         int    `json:"read_pause_ms"`
	SlowIntervalS       int    `json:"slow_interval_s"`
	ForwardUnknownReads bool   `json:"forward_unknown_reads"`
	LogLevel            string `json:"log_level"`
	CacheTTLH           int    `json:"cache_ttl_h"`
	HasBattery          bool   `json:"has_battery"`
	HasPowerMeter       bool   `json:"has_power_meter"`
}

// LoadHAOptions reads and validates the add-on's JSON options. The generated
// configuration enables read-only mode and adds groups for enabled hardware.
func LoadHAOptions(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading HA options: %w", err)
	}

	opts := &HAOptions{
		InverterPort:        502,
		UnitIDs:             []int{1},
		ReadPauseMs:         500,
		SlowIntervalS:       300,
		ForwardUnknownReads: true,
		LogLevel:            "info",
		CacheTTLH:           2,
	}

	if err := json.Unmarshal(data, opts); err != nil {
		return nil, fmt.Errorf("parsing HA options: %w", err)
	}

	if opts.InverterHost == "" {
		return nil, fmt.Errorf("inverter_host is required")
	}

	if len(opts.UnitIDs) == 0 {
		return nil, fmt.Errorf("unit_ids: at least one unit ID is required")
	}
	// Validate IDs before narrowing them to the one-byte Modbus unit field.
	unitIDs := make([]byte, len(opts.UnitIDs))
	for i, id := range opts.UnitIDs {
		if id < 1 || id > 247 {
			return nil, fmt.Errorf("unit_ids: %d out of range 1-247", id)
		}
		unitIDs[i] = byte(id)
	}

	cfg := &Config{
		ReadOnly: true,
		Inverter: InverterConfig{
			WaitAfterConnectMs: 1000,
			Host:               opts.InverterHost,
			Port:               opts.InverterPort,
			UnitIDs:            unitIDs,
			TimeoutMs:          5000,
		},
		Server: ServerConfig{
			Listen: ":502",
		},
		Web: WebConfig{
			Listen:      "127.0.0.1:8080",
			AuthEnabled: true,
		},
		Polling: PollingConfig{
			FastIntervalS: 5,
			FastMaxAgeS:   30,
			SlowMaxAgeS:   900,
			ReadPauseMs:   opts.ReadPauseMs,
			SlowIntervalS: opts.SlowIntervalS,
		},
		ForwardUnknownReads: opts.ForwardUnknownReads,
		RegisterGroups:      defaultRegisterGroups(opts.HasBattery, opts.HasPowerMeter),
		LogLevel:            opts.LogLevel,
		CachePath:           "/data/cache.db",
		CacheTTLH:           opts.CacheTTLH,
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// defaultRegisterGroups builds the add-on's polling map. It includes the
// inverter ranges and adds meter and battery ranges when enabled.
func defaultRegisterGroups(hasBattery, hasPowerMeter bool) []RegisterGroup {
	groups := []RegisterGroup{
		// Operational data uses the fast polling interval.
		{Name: "inverter_state", Address: 32000, Count: 11, Poll: "fast"},
		{Name: "pv_strings", Address: 32016, Count: 48, Poll: "fast"},
		{Name: "grid_power", Address: 32064, Count: 33, Poll: "fast"},
		{Name: "energy_yield", Address: 32106, Count: 14, Poll: "fast"},

		// Configuration and rating values use the slow polling interval.
		// Omitted startup or diagnostic ranges can be fetched on demand when
		// forward_unknown_reads is enabled.
		{Name: "rated_power", Address: 30073, Count: 4, Poll: "slow"},
		{Name: "optimizer_info", Address: 37200, Count: 2, Poll: "slow"},
		{Name: "inverter_config", Address: 40122, Count: 10, Poll: "slow"},
		{Name: "grid_mppt_config", Address: 42054, Count: 4, Poll: "slow"},
		{Name: "power_control", Address: 47415, Count: 4, Poll: "slow"},
	}

	if hasPowerMeter {
		groups = append(groups,
			RegisterGroup{Name: "meter", Address: 37100, Count: 39, Poll: "fast"},
		)
	}

	if hasBattery {
		// Fast battery groups
		groups = append(groups,
			RegisterGroup{Name: "storage_unit_1", Address: 37000, Count: 70, Poll: "fast"},
			RegisterGroup{Name: "storage_aggregated", Address: 37758, Count: 30, Poll: "fast"},
		)

		// Slow battery groups
		groups = append(groups,
			RegisterGroup{Name: "storage_unit_2", Address: 37700, Count: 58, Poll: "slow"},
			RegisterGroup{Name: "battery_soh", Address: 37920, Count: 8, Poll: "slow"},
			RegisterGroup{Name: "battery_packs_1", Address: 38200, Count: 84, Poll: "slow"},
			RegisterGroup{Name: "battery_packs_2", Address: 38284, Count: 84, Poll: "slow"},
			RegisterGroup{Name: "battery_packs_3", Address: 38368, Count: 96, Poll: "slow"},
			RegisterGroup{Name: "storage_config_1", Address: 47000, Count: 109, Poll: "slow"},
			RegisterGroup{Name: "storage_config_2", Address: 47200, Count: 100, Poll: "slow"},
			RegisterGroup{Name: "capacity_control", Address: 47954, Count: 67, Poll: "slow"},
		)
	}

	return groups
}
