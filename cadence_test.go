package main

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// cadenceReader supplies independent units and both polling tiers so the
// tests can inject timestamps without a running poller.
func cadenceReader() *Reader {
	return &Reader{cfg: &Config{
		Inverter: InverterConfig{UnitIDs: []byte{1, 2}},
		Polling:  PollingConfig{FastMaxAgeS: 30, SlowMaxAgeS: 900},
		RegisterGroups: []RegisterGroup{
			{Name: "inverter", Address: 32080, Count: 2, Poll: "fast"},
			{Name: "meter", Address: 37113, Count: 2, Poll: "fast"},
			{Name: "info", Address: 30000, Count: 2, Poll: "slow"},
		},
	}}
}

func TestCadenceMedianRecommendationAndIsolation(t *testing.T) {
	r := cadenceReader()
	now := time.Now()
	stamp := now.Add(-100 * time.Second)
	r.cadence.record(1, "inverter", stamp)
	for i, gap := range []time.Duration{4, 5, 4, 5, 4} {
		stamp = stamp.Add(gap * time.Second)
		r.cadence.record(1, "inverter", stamp)
		stat := r.CadenceStats(stamp)[0]
		if i < 4 && stat.RecommendedHASecs != 0 {
			t.Fatal("recommended before five intervals")
		}
	}
	// A seventh observation gives six intervals, so the median averages the
	// middle pair. The longer gap represents time between successful polls.
	stamp = stamp.Add(25 * time.Second)
	r.cadence.record(1, "inverter", stamp)
	r.cadence.record(2, "inverter", stamp)
	r.cadence.record(1, "meter", stamp)
	stats := r.CadenceStats(stamp)
	if s := stats[0]; s.Samples != 6 || s.Median != 4500*time.Millisecond || s.Maximum != 25*time.Second || s.LastGap != 25*time.Second || s.RecommendedHASecs != 2 {
		t.Fatalf("incorrect cadence: %+v", s)
	}
	if stats[1].Samples != 0 || stats[3].Samples != 0 {
		t.Fatal("mixed units or groups")
	}
	if s := r.CadenceStats(stamp.Add(31 * time.Second))[0]; !s.Stale || s.RecommendedHASecs != 0 || s.Samples != 6 {
		t.Fatalf("outage must suppress recommendation without inventing samples: %+v", s)
	}
	if s := r.CadenceStats(stamp.Add(-time.Second))[0]; !s.Stale || s.RecommendedHASecs != 0 {
		t.Fatal("future observation accepted")
	}
	if s := cadenceReader().CadenceStats(stamp)[0]; s.Samples != 0 || !s.LastSuccess.IsZero() || s.RecommendedHASecs != 0 {
		t.Fatal("new reader must start without historical samples")
	}
}

func TestCadenceRollingWindowAndMinimum(t *testing.T) {
	r := cadenceReader()
	stamp := time.Now()
	r.cadence.record(1, "inverter", stamp)
	// The first five intervals must disappear when the ring wraps.
	for i := 0; i < 105; i++ {
		gap := 500 * time.Millisecond
		if i < 5 {
			gap = time.Hour
		}
		stamp = stamp.Add(gap)
		r.cadence.record(1, "inverter", stamp)
	}
	r.cadence.record(1, "inverter", stamp) // duplicate ignored
	r.cadence.record(1, "inverter", stamp.Add(-time.Second))
	if s := r.CadenceStats(stamp)[0]; s.Samples != 100 || s.Median != 500*time.Millisecond || s.Maximum != 500*time.Millisecond || s.RecommendedHASecs != 1 {
		t.Fatalf("window did not evict old intervals or clamp recommendation: %+v", s)
	}
	for i := 0; i <= 5; i++ {
		r.cadence.record(1, "info", stamp.Add(time.Duration(i)*300*time.Second))
	}
	if s := r.CadenceStats(stamp.Add(1500 * time.Second))[2]; s.Samples != 5 || s.Stale || s.RecommendedHASecs != 0 {
		t.Fatal("slow group must not receive fast HA recommendation")
	}
}

func TestCadenceConcurrentSnapshots(t *testing.T) {
	r := cadenceReader()
	now := time.Now()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 250; i++ {
			r.cadence.record(1, "inverter", now.Add(time.Duration(i)*time.Second))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 250; i++ {
			r.CadenceStats(now.Add(time.Duration(i) * time.Second))
		}
	}()
	wg.Wait()
}

func TestCadenceDashboard(t *testing.T) {
	cfg := testConfig(t)
	cfg.RegisterGroups = []RegisterGroup{{Name: "inverter", Address: 32080, Count: 2, Poll: "fast"}}
	cfg.Polling.FastMaxAgeS = 30
	r := NewReader(cfg, nil, nil)
	now := time.Now()
	for i := 0; i <= 5; i++ {
		r.cadence.record(1, "inverter", now.Add(-time.Duration(5-i)*4500*time.Millisecond))
	}
	w := NewWebServer(cfg, testCache(t))
	w.SetReader(r)
	response := httptest.NewRecorder()
	w.handleIndex(response, httptest.NewRequest("GET", "/", nil))
	for _, want := range []string{"Measured polling intervals", "4.50 s", "5 / 100", "2 s", "Suggested HA interval"} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
}

// TestBatchSetReportsPersistenceFailure uses a closed database to simulate
// a poll whose values cannot be persisted.
func TestBatchSetReportsPersistenceFailure(t *testing.T) {
	c := testCache(t)
	if err := c.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.BatchSet([]PendingWrite{{UnitID: 1, Address: 32080, Values: []uint16{1, 2}}}); err == nil {
		t.Fatal("cache failure must be distinguishable from a successful acquisition")
	}
}
