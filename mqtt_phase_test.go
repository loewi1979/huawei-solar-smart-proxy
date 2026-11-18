package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMeasuredPhasePayload(t *testing.T) {
	c := testCache(t)
	m := defaultMQTTConfig()
	c.Set(1, 32080, []uint16{0, 9000})
	c.Set(1, 32106, []uint16{0, 12345})
	c.Set(1, 32069, []uint16{2307, 2312, 2298})
	c.Set(1, 32072, []uint16{0, 12345, 0xffff, 0xfc18, 0, 0}) // 12.345A, -1A, measured zero
	c.Set(1, 32085, []uint16{5001})
	decode := func() map[string]json.RawMessage {
		t.Helper()
		data, err := victronPayload(c, m, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		var wire struct {
			PV map[string]json.RawMessage `json:"pv"`
		}
		if err := json.Unmarshal(data, &wire); err != nil {
			t.Fatal(err)
		}
		if len(wire.PV) != 5 {
			t.Fatalf("unexpected aggregate keys: %s", data)
		}
		return wire.PV
	}
	pv := decode()
	for i, phaseName := range []string{"L1", "L2", "L3"} {
		var phase map[string]float64
		if err := json.Unmarshal(pv[phaseName], &phase); err != nil {
			t.Fatal(err)
		}
		if len(phase) != 3 || phase["voltage"] != []float64{230.7, 231.2, 229.8}[i] ||
			phase["current"] != []float64{12.345, -1, 0}[i] || phase["frequency"] != 50.01 {
			t.Fatalf("incorrect or fabricated phase fields: %s", pv[phaseName])
		}
	}
	// Omit one stale/invalid field while retaining independent fresh measurements.
	c.BatchSet([]PendingWrite{{UnitID: 1, Address: 32069, Values: []uint16{2307}, UpdatedAt: time.Now().Add(-31 * time.Second)}})
	c.Set(1, 32074, []uint16{0x8000, 0})
	c.Set(1, 32071, []uint16{0xffff})
	c.Set(1, 32085, []uint16{0xffff})
	pv = decode()
	for _, phaseName := range []string{"L1", "L2", "L3"} {
		var phase map[string]float64
		json.Unmarshal(pv[phaseName], &phase)
		if len(phase) != 1 {
			t.Fatalf("stale/sentinel field published: %s", pv[phaseName])
		}
	}
	var l2 map[string]float64
	json.Unmarshal(pv["L2"], &l2)
	if l2["voltage"] != 231.2 {
		t.Fatal("independent voltage lost")
	}
	// A different Modbus unit must never borrow phase values from unit 1.
	m.UnitID = 2
	c.Set(2, 32080, []uint16{0, 42})
	c.Set(2, 32106, []uint16{0, 100})
	data, err := victronPayload(c, m, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		PV map[string]json.RawMessage `json:"pv"`
	}
	json.Unmarshal(data, &wire)
	if len(wire.PV) != 2 {
		t.Fatal("phase data leaked across units")
	}
}
