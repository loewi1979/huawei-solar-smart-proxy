# Huawei Solar Smart Proxy

Standalone Go Modbus TCP caching proxy for Huawei SUN2000-10KTL-M1 via SDongleA-05, with SQLite, prioritized polling, a configuration UI and optional MQTT for Victron. Based on [siebertm/huawei-proxy](https://github.com/siebertm/huawei-proxy) (MIT); the upstream Git history is preserved.

**Development fork: Linux, hardware and GX acceptance remain pending. No production configuration has been changed.**

## Features

- Retains upstream SQLite WAL, multi-unit Modbus client, serial request handling, inter-request pauses, register map and dashboard.
- Configurable fast/slow intervals and group priority. Fast jobs precede slow jobs between requests; each group has its own acquisition timestamp.
- Per-read freshness checks, including persisted data after restart. Retention is independent of freshness. Failed polls never rejuvenate old data. Inverter absence does not prevent startup or recovery.
- Read-only defaults. Write and unknown-read forwarding require explicit settings. Modbus is unauthenticated and needs network isolation.
- Optional embedded MQTT broker with credentials, read-only subscribers and optional TLS; alternatively an external broker with validated TLS. Non-retained QoS 0 measurements with no persisted measurement queue.
- Victron JSON topic `huawei_wechselrichter`: measured total active power and total yield only. No invented phase power factors or energy counters.
- Authenticated YAML editor, backup download, restore upload, strict validation, atomic replacement, previous-file rollback, offline validation/rollback commands. Saved settings activate on explicit restart.
- Native/systemd and Docker documentation, automated failure tests, vulnerability scanning and CI.

## Local test

Use Go 1.27.4 or newer:

```sh
go test -race -count=1 ./...
go vet ./...
go build -o huawei-solar-proxy .
cp config.sun2000-m1.example.yaml config.yaml
./huawei-solar-proxy -config config.yaml -check-config
./huawei-solar-proxy -config config.yaml
```

The example points to a loopback test inverter. Open http://127.0.0.1:8080/ for status and `/config` for YAML management. Set a random `web.token` of at least 24 characters to enable configuration management (username `admin`). Keep actual credentials and addresses out of Git.

## Victron caveat

The stock mr-manuel driver copies aggregate values to L1 when phase powers are absent. That is not a phase measurement. A separately reviewable aggregate-only companion patch is included; GX/ESS behavior with unavailable phase paths must be tested before deployment. On silence, the receiver exits only after its nonzero timeout; `timeout = 0` can preserve stale values indefinitely.

## Documentation

- [Deployment, configuration, rollback and controlled rollout](docs/DEPLOYMENT.md)
- [Victron payload, exact driver behavior and receiver tests](docs/VICTRON.md)
- [Upstream baseline and initial risks](docs/UPSTREAM_REVIEW.md)
- [Security review and remaining limits](docs/SECURITY_REVIEW.md)
- [Preserved upstream README](docs/UPSTREAM_README.md)
- [Original register map](REGISTERS.md)

MIT; original license and attribution are retained.
