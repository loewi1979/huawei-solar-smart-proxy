package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMeterProfileCoverageAndDashboard(t *testing.T) {
	cfg, err := LoadConfig("config.sun2000-m1.meter.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Required HA readings: status/alarms, PV, phase diagnostics, power,
	// lifetime energy, daily/monthly/yearly yield, meter and configuration.
	for _, span := range [][2]uint16{{32000, 11}, {32016, 4}, {32064, 31}, {32106, 14}, {37100, 39}, {30073, 4}, {40125, 3}, {42054, 4}, {47415, 10}} {
		if !cfg.KnownRange(span[0], span[1]) {
			t.Fatalf("required range missing: %v", span)
		}
	}
	if cfg.MaxAge(32114, 2) != 900*time.Second || cfg.MaxAge(32080, 2) != 30*time.Second || cfg.MaxAge(37113, 2) != 30*time.Second {
		t.Fatal("daily yield must use slow freshness; PV/grid power must use fast freshness")
	}
	for _, address := range []uint16{37000, 37760, 37765, 38200, 47000, 47200, 47604, 47954} {
		if cfg.KnownRange(address, 1) {
			t.Fatalf("unexpected battery/backup range at %d", address)
		}
	}
	c := testCache(t)
	c.Set(1, 32114, []uint16{0, 1234})
	c.Set(1, 32089, []uint16{0x0200})
	response := httptest.NewRecorder()
	NewWebServer(cfg, c).handleIndex(response, httptest.NewRequest("GET", "/", nil))
	body := response.Body.String()
	for _, expected := range []string{"Daily Yield", "12.34 kWh", "On-grid"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("dashboard missing %q", expected)
		}
	}
	if strings.Contains(body, "Battery SOC") || strings.Contains(body, "Battery Charge Today") {
		t.Fatal("unconfigured battery widgets shown")
	}
}
