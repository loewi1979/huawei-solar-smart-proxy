package main

// Polling cadence measures how often the poller acquires and persists fresh
// data for each unit and group. The dashboard uses it to suggest HA intervals.

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

// cadenceWindow retains the latest intervals in a fixed-size ring. Intervals
// measure the gap between acquisition timestamps after successful commits.
// Only background polls contribute. Unchanged values count, and the next
// sample includes any time spent on failed polls or reconnecting.
type cadenceWindow struct {
	last      time.Time
	intervals [cadenceWindowSize]time.Duration
	count     int
	next      int
	lastGap   time.Duration
}

// pollingCadence holds the reader's in-memory history. Its mutex guards the
// map and each window so recording and dashboard snapshots can run concurrently.
type pollingCadence struct {
	mu      sync.Mutex
	windows map[cadenceKey]*cadenceWindow
}

// record observes a committed poll. The first successful acquisition sets the
// baseline, and later acquisitions contribute only positive intervals.
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

// PollingCadenceStat summarizes the rolling window for one unit and group.
// A recommendation requires a fresh fast group with at least five intervals.
// RecommendedHASecs is zero for all other groups.
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

// CadenceStats returns one snapshot per unit and group in configuration order.
// The bounded history starts empty after each restart.
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
					// Sort a copy to preserve the ring's overwrite order.
					intervals := append([]time.Duration(nil), w.intervals[:w.count]...)
					sort.Slice(intervals, func(i, j int) bool { return intervals[i] < intervals[j] })
					middle := len(intervals) / 2
					stat.Median = intervals[middle]
					if len(intervals)%2 == 0 {
						lower := intervals[middle-1]
						stat.Median = lower + (intervals[middle]-lower)/2
					}
					stat.Maximum = intervals[len(intervals)-1]
					// Suggest half the measured median rounded down to whole seconds,
					// with a minimum of one second.
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
