package main

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCombinedFastSlowModbusRead(t *testing.T) {
	cfg, err := LoadConfig("config.sun2000-m1.meter.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c := testCache(t)
	now := time.Now()
	c.BatchSet([]PendingWrite{
		{UnitID: 1, Address: 32106, Values: []uint16{0, 123}, UpdatedAt: now.Add(-time.Second)},
		{UnitID: 1, Address: 32108, Values: make([]uint16, 12), UpdatedAt: now.Add(-3 * time.Minute)},
	})
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer b.Close()
	go NewServer(cfg, c).handleConnection(ctx, a)
	read := func() []byte {
		t.Helper()
		b.SetDeadline(time.Now().Add(time.Second))
		// A client batches lifetime and daily/monthly/yearly energy together.
		if _, err := b.Write([]byte{0, 1, 0, 0, 0, 6, 1, 3, 0x7d, 0x6a, 0, 14}); err != nil {
			t.Fatal(err)
		}
		header := make([]byte, 7)
		if _, err := io.ReadFull(b, header); err != nil {
			t.Fatal(err)
		}
		pdu := make([]byte, int(binary.BigEndian.Uint16(header[4:6]))-1)
		if _, err := io.ReadFull(b, pdu); err != nil {
			t.Fatal(err)
		}
		return pdu
	}
	if pdu := read(); len(pdu) != 30 || pdu[0] != 3 || pdu[1] != 28 {
		t.Fatalf("valid mixed-age read rejected: %x", pdu)
	}
	c.BatchSet([]PendingWrite{{UnitID: 1, Address: 32106, Values: []uint16{0, 123}, UpdatedAt: now.Add(-31 * time.Second)}})
	if pdu := read(); len(pdu) != 2 || pdu[1] != 0x0b {
		t.Fatalf("slow allowance hid stale fast data: %x", pdu)
	}
	c.Set(1, 32106, []uint16{0, 123})
	c.BatchSet([]PendingWrite{{UnitID: 1, Address: 32108, Values: make([]uint16, 12), UpdatedAt: now.Add(-901 * time.Second)}})
	if pdu := read(); len(pdu) != 2 || pdu[1] != 0x0b {
		t.Fatalf("expired slow data served: %x", pdu)
	}
}

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
