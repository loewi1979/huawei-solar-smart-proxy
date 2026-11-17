# Upstream review — 2026-11-17

Baseline: siebertm/huawei-proxy main c02f0165110e8f2e2476d54f42a5be5ca12252e6 (25 commits, MIT). Real GitHub fork: loewi1979/huawei-solar-smart-proxy. No open upstream issues or PRs returned by GitHub on review date.

## Reuse
SQLite (pure Go, WAL, per-unit registers, atomic batch updates), serialized Modbus client, pauses, retries, multi-unit support, register decoder, dashboard, YAML and HA packaging.

## Findings before implementation
- HIGH: cache reads ignore updated_at. Two-hour eviction is retention, not freshness; starts serving persisted values before inverter connection and eviction.
- HIGH: read/write Modbus service and dashboard listen on all interfaces without authentication; downstream clients can change inverter settings.
- HIGH: slow polling plus retries blocks fast reads; a batch timestamp assigned at commit makes earlier reads appear newer than their actual acquisition.
- MEDIUM: no automated tests, no MQTT, no validated config backup/restore, no rollback.
- MEDIUM: startup exits on unavailable inverter; no HTTP read-header timeout; Modbus peers can hold connections indefinitely; range overflow and write count bounds incomplete.
- MEDIUM: undocumented differences between HA options and standalone YAML; README and CLAUDE.md still mention RAM cache.

## Acceptance / rollout
Develop with local protocol simulator and temporary SQLite only. No production HA/GX settings or direct dongle access without explicit coordination. Preserve FusionSolar; a parallel Modbus connection may conflict with the existing reader. Hardware, dongle firmware, actual register availability and GX driver version must be recorded before deployment.

Freshness must be checked on every consumer read, including MQTT, and use actual successful acquisition timestamps. Never refresh old timestamps on failed polls. Return Modbus failure for configured stale ranges. MQTT data must be non-retained and stop on stale data; receiver-side silence handling must be independently verified. Do not synthesize phase powers from total PF, or divide total energy over phases.
