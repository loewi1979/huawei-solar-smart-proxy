package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

const (
	maxRetries        = 3
	deviceBusyBackoff = 3 * time.Second
	reconnectBackoff  = 2 * time.Second
	initialScanPasses = 3
)

// ReaderStats contains runtime statistics from the polling loop.
type ReaderStats struct {
	CycleCount    int64
	LastCycleTime time.Time
	LastCycleDur  time.Duration
}

// Reader continuously polls register groups from the inverter
// and updates the cache. Fast groups are read every cycle, slow
// groups only when their interval has elapsed.
type Reader struct {
	cfg        *Config
	client     *InverterClient
	cache      *RegisterCache
	cadence    pollingCadence
	fastGroups []RegisterGroup
	slowGroups []RegisterGroup

	cycleCount    atomic.Int64
	lastCycleTime atomic.Value // time.Time
	lastCycleDur  atomic.Int64 // nanoseconds
}

func NewReader(cfg *Config, client *InverterClient, cache *RegisterCache) *Reader {
	r := &Reader{
		cfg:    cfg,
		client: client,
		cache:  cache,
	}

	for _, g := range cfg.RegisterGroups {
		switch g.Poll {
		case "fast":
			r.fastGroups = append(r.fastGroups, g)
		case "slow":
			r.slowGroups = append(r.slowGroups, g)
		}
	}

	return r
}

func (r *Reader) readGroup(ctx context.Context, unitID byte, g RegisterGroup) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		data, err := r.client.ReadRegisters(unitID, g.Address, g.Count)
		if err == nil {
			slog.Debug("read register group",
				"unit_id", unitID,
				"name", g.Name,
				"address", g.Address,
				"count", g.Count,
			)
			return data, nil
		}

		lastErr = err

		if IsDeviceBusy(err) || IsTimeout(err) {
			if attempt < maxRetries {
				slog.Warn("retryable error, retrying after backoff",
					"name", g.Name,
					"attempt", attempt,
					"backoff", deviceBusyBackoff,
					"error", err,
				)
				if err := sleepCtx(ctx, deviceBusyBackoff); err != nil {
					return nil, err
				}
				continue
			}
		} else if IsTransactionMismatch(err) || IsConnectionError(err) {
			if attempt < maxRetries {
				slog.Warn("connection error, reconnecting",
					"name", g.Name,
					"attempt", attempt,
					"error", err,
				)
				r.client.Reconnect()
				if err := sleepCtx(ctx, reconnectBackoff); err != nil {
					return nil, err
				}
				continue
			}
		} else {
			// Non-retryable error — return immediately
			break
		}
	}

	// If all retries exhausted on a retryable error, reconnect so the next
	// read starts with a fresh TCP connection instead of looping on a dead one.
	if IsDeviceBusy(lastErr) || IsTimeout(lastErr) || IsTransactionMismatch(lastErr) || IsConnectionError(lastErr) {
		slog.Warn("all retries exhausted, reconnecting",
			"name", g.Name,
			"error", lastErr,
		)
		r.client.Reconnect()
	}

	return nil, fmt.Errorf("reading %s (addr=%d, count=%d): %w", g.Name, g.Address, g.Count, lastErr)
}

// sleepCtx sleeps for the given duration, returning early if the context
// is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// unitGroup pairs a unit ID with a register group for initial scan tracking.
type unitGroup struct {
	unitID byte
	group  RegisterGroup
}

// InitialScan reads all groups (fast and slow) for all unit IDs to populate
// the cache before the server starts accepting connections. Groups that fail
// are retried in additional passes, which handles inverter warm-up after TCP connect.
func (r *Reader) InitialScan(ctx context.Context) error {
	allGroups := make([]RegisterGroup, 0, len(r.fastGroups)+len(r.slowGroups))
	allGroups = append(allGroups, r.fastGroups...)
	allGroups = append(allGroups, r.slowGroups...)

	var pending []unitGroup
	for _, g := range allGroups {
		for _, uid := range r.cfg.Inverter.UnitIDs {
			pending = append(pending, unitGroup{unitID: uid, group: g})
		}
	}

	for pass := 1; pass <= initialScanPasses; pass++ {
		var failed []unitGroup
		var batch []PendingWrite
		for _, ug := range pending {
			data, err := r.readGroup(ctx, ug.unitID, ug.group)
			if err != nil {
				slog.Warn("initial scan: failed to read group",
					"unit_id", ug.unitID,
					"name", ug.group.Name,
					"address", ug.group.Address,
					"pass", pass,
					"error", err,
				)
				failed = append(failed, ug)
			} else {
				batch = append(batch, PendingWrite{
					UnitID:  ug.unitID,
					Address: ug.group.Address,
					Values:  bytesToUint16(data),
				})
			}
		}
		r.cache.BatchSet(batch)
		if len(failed) == 0 {
			break
		}
		pending = failed
		if pass < initialScanPasses {
			slog.Info("initial scan: retrying failed groups",
				"count", len(failed),
				"pass", pass+1,
			)
		}
	}

	slog.Info("initial scan complete", "cached_registers", r.cache.Size())
	return nil
}

// Run starts the continuous reader loop. It reads fast groups every cycle
// and slow groups when their interval has elapsed.
func (r *Reader) Run(ctx context.Context) {
	type job struct {
		group RegisterGroup
		unit  byte
		due   time.Time
	}
	var jobs []job
	for _, g := range r.cfg.RegisterGroups {
		for _, u := range r.cfg.Inverter.UnitIDs {
			jobs = append(jobs, job{group: g, unit: u})
		}
	}
	lastClean := time.Now()
	for ctx.Err() == nil {
		if time.Since(lastClean) >= r.cfg.SlowInterval() {
			r.cache.DeleteStale(r.cfg.CacheTTL())
			lastClean = time.Now()
		}
		now := time.Now()
		chosen := -1
		wake := now.Add(time.Second)
		for i, j := range jobs {
			if j.due.After(now) {
				if j.due.Before(wake) {
					wake = j.due
				}
				continue
			}
			if chosen < 0 || (j.group.Poll == "fast" && jobs[chosen].group.Poll != "fast") ||
				(j.group.Poll == jobs[chosen].group.Poll && (j.group.Priority > jobs[chosen].group.Priority || j.group.Priority == jobs[chosen].group.Priority && j.due.Before(jobs[chosen].due))) {
				chosen = i
			}
		}
		if chosen < 0 {
			if sleepCtx(ctx, time.Until(wake)) != nil {
				return
			}
			continue
		}
		j := &jobs[chosen]
		started := time.Now()
		data, err := r.client.ReadRegisters(j.unit, j.group.Address, j.group.Count)
		if err == nil && len(data) == int(j.group.Count)*2 {
			acquiredAt := time.Now()
			if r.cache.BatchSet([]PendingWrite{{UnitID: j.unit, Address: j.group.Address, Values: bytesToUint16(data), UpdatedAt: acquiredAt}}) == nil {
				r.cadence.record(j.unit, j.group.Name, acquiredAt)
			}
		} else {
			slog.Warn("poll failed", "group", j.group.Name, "unit", j.unit, "error", err)
			r.client.Reconnect()
		}
		interval := r.cfg.SlowInterval()
		if j.group.Poll == "fast" {
			interval = time.Duration(r.cfg.Polling.FastIntervalS) * time.Second
		}
		j.due = time.Now().Add(interval)
		r.cycleCount.Add(1)
		r.lastCycleTime.Store(time.Now())
		r.lastCycleDur.Store(int64(time.Since(started)))
	}
}

// Stats returns current runtime statistics.
func (r *Reader) Stats() ReaderStats {
	var lastTime time.Time
	if v := r.lastCycleTime.Load(); v != nil {
		lastTime = v.(time.Time)
	}
	return ReaderStats{
		CycleCount:    r.cycleCount.Load(),
		LastCycleTime: lastTime,
		LastCycleDur:  time.Duration(r.lastCycleDur.Load()),
	}
}

// FastGroupCount returns the number of fast polling groups.
func (r *Reader) FastGroupCount() int { return len(r.fastGroups) }

// SlowGroupCount returns the number of slow polling groups.
func (r *Reader) SlowGroupCount() int { return len(r.slowGroups) }
