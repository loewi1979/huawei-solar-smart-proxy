package main

import (
	"sort"
	"sync"
	"time"
)

const cadenceWindowSize = 100
const cadenceRecommendationMinimum = 5

type cadenceKey struct {
	unit  byte
	group string
}

// One interval ends only after a successful acquisition and cache commit.
// Unchanged values count; failed polls and downstream reads do not. The next
// successful interval includes any intervening failure/reconnect delay.
type cadenceWindow struct {
	last      time.Time
	intervals [cadenceWindowSize]time.Duration
	count     int
	next      int
	lastGap   time.Duration
}

type pollingCadence struct {
	mu      sync.Mutex
	windows map[cadenceKey]*cadenceWindow
}

func (p *pollingCadence) record(unit byte, group string, acquired time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if acquired.IsZero() {
		return
	}
	if p.windows == nil {
		p.windows = make(map[cadenceKey]*cadenceWindow)
	}
	key := cadenceKey{unit, group}
	w := p.windows[key]
	if w == nil {
		p.windows[key] = &cadenceWindow{last: acquired}
		return
	}
	gap := acquired.Sub(w.last)
	if gap <= 0 {
		return
	}
	w.intervals[w.next] = gap
	w.next = (w.next + 1) % cadenceWindowSize
	if w.count < cadenceWindowSize {
		w.count++
	}
	w.last, w.lastGap = acquired, gap
}

type PollingCadenceStat struct {
	UnitID            byte
	Group             string
	Poll              string
	Samples           int
	Median            time.Duration
	Maximum           time.Duration
	LastGap           time.Duration
	LastSuccess       time.Time
	Stale             bool
	RecommendedHASecs int
}

// Snapshot in configuration order, never combining different units or groups.
// The small, bounded windows are in memory and intentionally reset on restart.
func (r *Reader) CadenceStats(now time.Time) []PollingCadenceStat {
	r.cadence.mu.Lock()
	defer r.cadence.mu.Unlock()
	var stats []PollingCadenceStat
	for _, unit := range r.cfg.Inverter.UnitIDs {
		for _, group := range r.cfg.RegisterGroups {
			stat := PollingCadenceStat{UnitID: unit, Group: group.Name, Poll: group.Poll, Stale: true}
			if w := r.cadence.windows[cadenceKey{unit, group.Name}]; w != nil {
				stat.Samples, stat.LastSuccess, stat.LastGap = w.count, w.last, w.lastGap
				age := now.Sub(w.last)
				stat.Stale = age < 0 || age > r.cfg.MaxAge(group.Address, group.Count)
				if w.count > 0 {
					intervals := append([]time.Duration(nil), w.intervals[:w.count]...)
					sort.Slice(intervals, func(i, j int) bool { return intervals[i] < intervals[j] })
					middle := len(intervals) / 2
					stat.Median = intervals[middle]
					if len(intervals)%2 == 0 {
						lower := intervals[middle-1]
						stat.Median = lower + (intervals[middle]-lower)/2
					}
					stat.Maximum = intervals[len(intervals)-1]
					if group.Poll == "fast" && !stat.Stale && w.count >= cadenceRecommendationMinimum {
						stat.RecommendedHASecs = int(stat.Median / (2 * time.Second))
						if stat.RecommendedHASecs < 1 {
							stat.RecommendedHASecs = 1
						}
					}
				}
			}
			stats = append(stats, stat)
		}
	}
	return stats
}
