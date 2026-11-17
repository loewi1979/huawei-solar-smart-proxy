package main

import (
	"fmt"
	"time"
)

// Fresh returns one SQLite snapshot, rejecting missing, malformed, future or old
// timestamps. Retention/eviction never controls whether a value may be served.
func (c *RegisterCache) Fresh(unit byte, address, count uint16, maxAge time.Duration, now time.Time) ([]uint16, time.Time, error) {
	if count == 0 || uint32(address)+uint32(count) > 65536 || maxAge <= 0 {
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
		if err != nil || t.After(now) || now.Sub(t) > maxAge {
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
	if len(values) != int(count) {
		return nil, time.Time{}, fmt.Errorf("cache miss")
	}
	return values, oldest, nil
}

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
