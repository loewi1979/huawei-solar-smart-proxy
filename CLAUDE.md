# CLAUDE.md

Project context and instructions for AI assistants working on this codebase.

## Project Overview

**huawei-solar-proxy** is a Modbus TCP caching proxy for Huawei SUN2000 solar inverters, written in Go. It sits between Home Assistant (using the `huawei_solar` integration) and the inverter to provide instant cached responses.

## Architecture

- **Single Go package** (`package main`) with focused files:
  - `main.go` — entry point, wiring, signal handling
  - `config.go` — YAML config parsing and validation
  - `cache.go`, `freshness.go` — SQLite WAL cache and per-read acquisition-age checks
  - `client.go` — serialized Modbus client, request/connection pauses and upstream session generation
  - `reader.go` — due-job polling, with fast jobs ahead of slow jobs
  - `server.go` — cached reads, read-only policy, optional writes and allowlisted Huawei login forwarding
  - `mqtt.go` — measured aggregate publisher, embedded read-only-subscriber broker or external broker
  - `config_web.go`, `web.go` — authenticated YAML backup/restore/rollback and status UI

- **Key dependencies:**
  - `github.com/goburrow/modbus` — Modbus TCP client
  - `gopkg.in/yaml.v3` — YAML config
  - `log/slog` — structured logging (stdlib)

## Building

```bash
go build -o huawei-solar-proxy .
```

## Testing

No inverter is needed for the automated protocol, broker and cache tests:
```bash
go test -race -count=1 ./...
go vet ./...
go build ./...
```

Integration testing requires a real inverter or a Modbus TCP simulator.

## Key Design Decisions

- **Request and connection pauses**: Both apply after reconnects too. The M1 example uses a 1000ms request gap and 1500ms connection quiet period with a 15s timeout. Short dongle timeouts can create late-response/transaction-ID failures.
- **Two-tier polling**: Fast/slow intervals are minimum waits after completion, not hard real-time guarantees. A request is non-preemptible; slow overload can cause stale values.
- **Forward unknown reads**: Disabled by default. Known stale ranges are rejected instead of silently forwarded.
- **Write forwarding**: `read_only: true` by default, enforced at both server and client. In write mode, 0x06/0x10 and allowlisted Huawei login subcommands use the same serialized connection. Upstream exceptions must be preserved for HA's permission/login handling. A reconnect invalidates downstream Huawei authentication.
- **Freshness**: Successful acquisition timestamps are never replaced on failure. Disk retention is not freshness. MQTT publishes measured aggregate values only, non-retained, and becomes silent on staleness.
- **All registers are holding registers** (function code 0x03). The HA integration never uses input registers (0x04), but the proxy handles both identically for robustness.

## Register Map

See `REGISTERS.md` for the complete map of all 511 known SUN2000 registers with addresses, lengths, and groupings. This was extracted from the `huawei_solar` Python library v2.5.0.

## Config

- `config.example.yaml` has all known register groups with comments
- Users copy to `config.yaml` and set their inverter IP
- Groups for unused hardware (battery, meter) should be removed

## Code Style

- Standard Go formatting (`gofmt`)
- `slog` for all logging (JSON to stdout)
- No frameworks — stdlib + minimal dependencies
- Keep the single-package flat structure
