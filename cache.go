package main

// The SQLite cache persists register values and timestamps across restarts.
// Freshness checks apply at read time, independently of disk retention.

import (
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
)

// CachedRegister holds a raw register value and the timestamp stored with it.
type CachedRegister struct {
	UnitID    byte
	Address   uint16
	Value     uint16
	UpdatedAt time.Time
}

// PendingWrite describes a contiguous range for one unit. UpdatedAt is the
// acquisition time. A zero value lets BatchSet timestamp the range.
type PendingWrite struct {
	UpdatedAt time.Time
	UnitID    byte
	Address   uint16
	Values    []uint16
}

// RegisterCache is a SQLite-backed cache of Modbus register values.
// Each raw uint16 value is keyed by both Modbus unit ID and register address.
type RegisterCache struct {
	db              *sql.DB
	stmtUpsert      *sql.Stmt
	stmtGet         *sql.Stmt
	stmtCount       *sql.Stmt
	stmtAll         *sql.Stmt
	stmtDeleteStale *sql.Stmt
}

// NewRegisterCache opens the cache and prepares its schema. The returned
// cache owns the database handle until Close and uses one SQLite connection.
func NewRegisterCache(dbPath string) (*RegisterCache, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite db: %w", err)
	}

	// Limit this process to one SQLite connection to avoid lock contention
	// between its cache readers and writers.
	db.SetMaxOpenConns(1)

	// WAL with NORMAL synchronization trades some crash durability for
	// write throughput. Polling can repopulate the cache.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("exec %s: %w", pragma, err)
		}
	}

	// Legacy rows lack a unit ID, so they cannot be assigned to a unit
	// reliably. Rebuild the table and let the background poller fill it.
	var hasUnitID bool
	rows, err := db.Query(`PRAGMA table_info(registers)`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var cid int
			var name, typ string
			var notnull int
			var dfltValue *string
			var pk int
			if err := rows.Scan(&cid, &name, &typ, &notnull, &dfltValue, &pk); err == nil {
				if name == "unit_id" {
					hasUnitID = true
				}
			}
		}
		rows.Close()
	}

	if !hasUnitID {
		db.Exec(`DROP TABLE IF EXISTS registers`)
		slog.Info("cache: migrated schema (dropped old table without unit_id)")
	}

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS registers (
			unit_id    INTEGER NOT NULL,
			address    INTEGER NOT NULL,
			value      INTEGER NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (unit_id, address)
		)
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("create table: %w", err)
	}

	stmtUpsert, err := db.Prepare(`INSERT INTO registers (unit_id, address, value, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(unit_id, address) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("prepare upsert: %w", err)
	}

	stmtGet, err := db.Prepare(`SELECT address, value FROM registers WHERE unit_id = ? AND address >= ? AND address < ? ORDER BY address`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("prepare get: %w", err)
	}

	stmtCount, err := db.Prepare(`SELECT COUNT(*) FROM registers`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("prepare count: %w", err)
	}

	stmtAll, err := db.Prepare(`SELECT unit_id, address, value, updated_at FROM registers ORDER BY unit_id, address`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("prepare all: %w", err)
	}

	stmtDeleteStale, err := db.Prepare(`DELETE FROM registers WHERE updated_at < ?`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("prepare delete stale: %w", err)
	}

	return &RegisterCache{
		db:              db,
		stmtUpsert:      stmtUpsert,
		stmtGet:         stmtGet,
		stmtCount:       stmtCount,
		stmtAll:         stmtAll,
		stmtDeleteStale: stmtDeleteStale,
	}, nil
}

// Close releases database resources.
func (c *RegisterCache) Close() error {
	c.stmtUpsert.Close()
	c.stmtGet.Close()
	c.stmtCount.Close()
	c.stmtAll.Close()
	c.stmtDeleteStale.Close()
	return c.db.Close()
}

// All returns the stored rows ordered by unit ID, then address. It includes
// expired rows for inspection and uses a zero time for unreadable timestamps.
func (c *RegisterCache) All() []CachedRegister {
	rows, err := c.stmtAll.Query()
	if err != nil {
		slog.Warn("cache all: query", "error", err)
		return nil
	}
	defer rows.Close()

	var result []CachedRegister
	for rows.Next() {
		var unitID, addr, val int
		var updatedAt string
		if err := rows.Scan(&unitID, &addr, &val, &updatedAt); err != nil {
			slog.Warn("cache all: scan", "error", err)
			return nil
		}
		t, _ := time.Parse(time.RFC3339Nano, updatedAt)
		result = append(result, CachedRegister{
			UnitID:    byte(unitID),
			Address:   uint16(addr),
			Value:     uint16(val),
			UpdatedAt: t,
		})
	}
	return result
}

// Set stores a contiguous range for one unit in a transaction, timestamping
// values as they are written. Cache errors are logged for this best-effort update.
func (c *RegisterCache) Set(unitID byte, address uint16, values []uint16) {
	tx, err := c.db.Begin()
	if err != nil {
		slog.Warn("cache set: begin tx", "error", err)
		return
	}

	stmt := tx.Stmt(c.stmtUpsert)
	for i, v := range values {
		addr := int(address) + i
		if _, err := stmt.Exec(int(unitID), addr, int(v), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			tx.Rollback()
			slog.Warn("cache set: exec", "address", addr, "error", err)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		slog.Warn("cache set: commit", "error", err)
	}
}

// bytesToUint16 converts raw Modbus response bytes (big-endian) to uint16 register values.
func bytesToUint16(data []byte) []uint16 {
	values := make([]uint16, len(data)/2)
	for i := range values {
		values[i] = uint16(data[i*2])<<8 | uint16(data[i*2+1])
	}
	return values
}

// SetFromBytes stores register values from raw Modbus response bytes (big-endian).
func (c *RegisterCache) SetFromBytes(unitID byte, address uint16, data []byte) {
	c.Set(unitID, address, bytesToUint16(data))
}

// BatchSet writes multiple groups of register values in a single SQLite transaction.
// Each range keeps its acquisition time, or uses the batch time when UpdatedAt
// is zero. For a nonempty batch, a nil error confirms that the transaction committed.
func (c *RegisterCache) BatchSet(writes []PendingWrite) error {
	if len(writes) == 0 {
		return nil
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)

	tx, err := c.db.Begin()
	if err != nil {
		slog.Warn("cache batch set: begin tx", "error", err)
		return err
	}

	stmt := tx.Stmt(c.stmtUpsert)
	for _, w := range writes {
		stamp := now
		if !w.UpdatedAt.IsZero() {
			stamp = w.UpdatedAt.UTC().Format(time.RFC3339Nano)
		}
		for i, v := range w.Values {
			addr := int(w.Address) + i
			if _, err := stmt.Exec(int(w.UnitID), addr, int(v), stamp); err != nil {
				tx.Rollback()
				slog.Warn("cache batch set: exec", "unit_id", w.UnitID, "address", addr, "error", err)
				return err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		slog.Warn("cache batch set: commit", "error", err)
		return err
	}
	return nil
}

// Get retrieves a complete range for one unit without checking timestamps.
// It returns nil if any register is missing or the query fails.
func (c *RegisterCache) Get(unitID byte, address, count uint16) []uint16 {
	rows, err := c.stmtGet.Query(int(unitID), int(address), int(address)+int(count))
	if err != nil {
		slog.Warn("cache get: query", "address", address, "count", count, "error", err)
		return nil
	}
	defer rows.Close()

	result := make([]uint16, count)
	found := 0
	for rows.Next() {
		var addr, val int
		if err := rows.Scan(&addr, &val); err != nil {
			slog.Warn("cache get: scan", "error", err)
			return nil
		}
		idx := addr - int(address)
		if idx < 0 || idx >= int(count) {
			continue
		}
		result[idx] = uint16(val)
		found++
	}

	if found != int(count) {
		return nil // all-or-nothing: every register in the range must be cached
	}
	return result
}

// GetBytes encodes the complete range from Get in Modbus byte order.
// It returns nil when Get cannot supply the range.
func (c *RegisterCache) GetBytes(unitID byte, address, count uint16) []byte {
	values := c.Get(unitID, address, count)
	if values == nil {
		return nil
	}

	data := make([]byte, len(values)*2)
	for i, v := range values {
		data[i*2] = byte(v >> 8)
		data[i*2+1] = byte(v)
	}
	return data
}

// Size returns the number of cached registers.
func (c *RegisterCache) Size() int {
	var count int
	if err := c.stmtCount.QueryRow().Scan(&count); err != nil {
		slog.Warn("cache size: query", "error", err)
		return 0
	}
	return count
}

// DeleteStale evicts rows older than the retention period and returns the
// number removed. Serving decisions use the freshness checks in freshness.go.
func (c *RegisterCache) DeleteStale(maxAge time.Duration) int {
	cutoff := time.Now().UTC().Add(-maxAge).Format(time.RFC3339Nano)
	result, err := c.stmtDeleteStale.Exec(cutoff)
	if err != nil {
		slog.Warn("cache delete stale: exec", "error", err)
		return 0
	}
	n, _ := result.RowsAffected()
	return int(n)
}
