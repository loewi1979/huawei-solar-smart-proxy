package main

// Freshness is checked from the timestamp of each cached register at read time.
// The same query supplies the value and timestamp for every register in a range.

import (
	"fmt"
	"time"
)

// Fresh returns the complete range and its oldest timestamp under one age
// limit. Missing rows and malformed, future or expired timestamps fail the read.
func (c *RegisterCache) Fresh(unit byte, address, count uint16, maxAge time.Duration, now time.Time) ([]uint16, time.Time, error) {
	if maxAge <= 0 {
		return nil, time.Time{}, fmt.Errorf("invalid freshness request")
	}
	return c.fresh(unit, address, count, func(uint16) time.Duration { return maxAge }, now)
}

// FreshConfigured checks each register against its own polling group's age
// limit, even when one Modbus read spans both fast and slow groups. It returns
// the complete range and its oldest timestamp from a single SQLite snapshot.
func (c *RegisterCache) FreshConfigured(unit byte, address, count uint16, cfg *Config, now time.Time) ([]uint16, time.Time, error) {
	return c.fresh(unit, address, count, func(a uint16) time.Duration { return cfg.MaxAge(a, 1) }, now)
}

func (c *RegisterCache) fresh(unit byte, address, count uint16, ageFor func(uint16) time.Duration, now time.Time) ([]uint16, time.Time, error) {
	if count == 0 || uint32(address)+uint32(count) > 65536 {
		return nil, time.Time{}, fmt.Errorf("invalid freshness request")
	}
	rows, err := c.db.Query(`SELECT address,value,updated_at FROM registers WHERE unit_id=? AND address>=? AND address<? ORDER BY address`, int(unit), int(address), int(address)+int(count))
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()
	var values []uint16
	var oldest time.Time
	for rows.Next() {
		var addr, value int
		var stamp string
		if err := rows.Scan(&addr, &value, &stamp); err != nil {
			return nil, time.Time{}, err
		}
		t, err := time.Parse(time.RFC3339Nano, stamp)
		maxAge := ageFor(uint16(addr))
		if err != nil || maxAge <= 0 || t.After(now) || now.Sub(t) > maxAge {
			return nil, time.Time{}, fmt.Errorf("stale cache at %d", addr)
		}
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
		values = append(values, uint16(value))
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, err
	}
	// The query bounds and unique unit/address key make the row count a
	// completeness check for the whole range.
	if len(values) != int(count) {
		return nil, time.Time{}, fmt.Errorf("cache miss")
	}
	return values, oldest, nil
}

// MaxAge returns the slow limit for a range wholly inside a slow group.
// Other ranges use the fast limit. FreshConfigured calls it per register to
// apply the correct limit across group boundaries.
func (c *Config) MaxAge(address, count uint16) time.Duration {
	age := time.Duration(c.Polling.FastMaxAgeS) * time.Second
	for _, g := range c.RegisterGroups {
		if uint32(address) >= uint32(g.Address) && uint32(address)+uint32(count) <= uint32(g.Address)+uint32(g.Count) {
			if g.Poll == "slow" {
				return time.Duration(c.Polling.SlowMaxAgeS) * time.Second
			}
			return age
		}
	}
	return age
}

// KnownRange reports whether configured groups cover every requested register.
// Adjacent groups can cover a single request together.
func (c *Config) KnownRange(address, count uint16) bool {
	for i := uint32(address); i < uint32(address)+uint32(count); i++ {
		found := false
		for _, g := range c.RegisterGroups {
			if i >= uint32(g.Address) && i < uint32(g.Address)+uint32(g.Count) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
