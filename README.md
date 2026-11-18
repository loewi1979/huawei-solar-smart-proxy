# Huawei Solar Smart Proxy

Standalone Go Modbus TCP caching proxy for Huawei SUN2000-10KTL-M1 via SDongleA-05, with SQLite, prioritized polling, a configuration UI and optional MQTT for Victron. Based on [siebertm/huawei-proxy](https://github.com/siebertm/huawei-proxy) (MIT); the upstream Git history is preserved.

**Development fork: native Linux and initial read-only SDongle tests passed. Extended daytime testing and GX acceptance remain pending. HA and Victron settings have not been changed by this project.**

## Features

- Retains upstream SQLite WAL, multi-unit Modbus client, serial request handling, inter-request pauses, register map and dashboard.
- Configurable fast/slow intervals and group priority. Fast jobs precede slow jobs between requests; each group has its own acquisition timestamp.
- Per-read freshness checks, including persisted data after restart. Retention is independent of freshness. Failed polls never rejuvenate old data. Inverter absence does not prevent startup or recovery.
- Read-only defaults. Write and unknown-read forwarding require explicit settings. Modbus is unauthenticated and needs network isolation.
- `read_only: false` enables standard register writes and HA's Huawei login exchange. The default is `true`; the legacy `allow_writes` alias cannot be combined with it. Inverter permissions still apply.
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

For a trusted LAN, explicitly setting `web.auth_enabled: false` disables login
for every web endpoint, including configuration editing and YAML backups.
The default is `true`; a non-loopback listener then requires a >=24-character
token. MQTT authentication and the Modbus `read_only` policy are independent.

## MQTT and Victron

The proxy includes both an optional MQTT broker and a Victron JSON publisher.
With `mqtt.enabled: true` and `mqtt.embedded: true`, subscribers connect directly
to `mqtt.listen` with the configured username and password. Remote subscribers
can read only `mqtt.topic` and its `/status` topic; publishing is denied.
MQTT Explorer's default `#` and `$SYS/#` subscription filters are accepted;
delivery remains limited to the configured measurement and status topics.
With `mqtt.embedded: false`, the proxy instead publishes to an external
`mqtt.broker` (`tcp://` or certificate-validated `ssl://`). No additional broker
is needed in embedded mode. Use a strong password on shared networks; non-empty
credentials are required, and the password length is the operator's choice.

Default topic: `huawei_wechselrichter`. The wire payload is:

```json
{"pv":{"power":1234,"energy_forward":123.45}}
```

Values are measured total inverter active power in W and lifetime yield in kWh.
They are published at `mqtt.interval_s`, only while both samples meet
`mqtt.max_age_s`. Measurements use QoS 0, are not retained, and stop when stale;
the separate `/status` topic reports availability. Smartmeter grid flow is a
separate measurement and is not substituted for inverter PV power.

### Victron caveat

The stock mr-manuel driver copies aggregate values to L1 when phase powers are absent. That is not a phase measurement. A separately reviewable aggregate-only companion patch is included; GX/ESS behavior with unavailable phase paths must be tested before deployment. On silence, the receiver exits only after its nonzero timeout; `timeout = 0` can preserve stale values indefinitely.

## Documentation

- [Deployment, configuration, rollback and controlled rollout](docs/DEPLOYMENT.md)
- [Victron payload, exact driver behavior and receiver tests](docs/VICTRON.md)
- [Fast-poll tuning and optional contiguous-read example](docs/FAST_POLLING.md)
- [HA inventory coverage and inverter/meter polling profile](docs/POLLING_PROFILE.md)
- [Upstream baseline and initial risks](docs/UPSTREAM_REVIEW.md)
- [Security review and remaining limits](docs/SECURITY_REVIEW.md)
- [Preserved upstream README](docs/UPSTREAM_README.md)
- [Original register map](REGISTERS.md)

MIT; original license and attribution are retained.
