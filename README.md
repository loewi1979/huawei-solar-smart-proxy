# Huawei Solar Smart Proxy

A standalone Go Modbus TCP caching proxy and MQTT publisher for Huawei SUN2000
solar inverters via SDongle. Connect **Home Assistant, Victron Venus OS, or both**
through one shared inverter connection:

- **Home Assistant:** Modbus TCP through the
  [wlcrs/huawei_solar integration](https://github.com/wlcrs/huawei_solar).
- **Victron Venus OS:** MQTT JSON designed specifically for
  [mr-manuel/venus-os_dbus-mqtt-pv](https://github.com/mr-manuel/venus-os_dbus-mqtt-pv).

Use either output independently or both together. An embedded MQTT broker is
included; an existing external broker can be used instead. FusionSolar remains
connected alongside the proxy.

Based on [siebertm/huawei-proxy](https://github.com/siebertm/huawei-proxy) (MIT).
The upstream Git history, license and attribution are preserved.

## Features

- SQLite WAL register cache, multi-unit support and one serialized upstream
  connection with configurable request and connection pauses.
- Prioritized Fast/Slow polling with configurable register groups. All groups
  are due at startup, with fast groups taking precedence between requests.
- Per-register freshness checks, including combined Fast/Slow reads and cached
  data after restart. Failed polls do not refresh timestamps.
- Live acquisition-interval medians over the last 100 intervals per unit/group,
  with suggested Home Assistant polling intervals on the dashboard.
- **Home Assistant integration via Modbus TCP:** cached reads, HA device
  discovery and configurable write forwarding for inverter controls.
- **MQTT publisher specifically for
  [venus-os_dbus-mqtt-pv](https://github.com/mr-manuel/venus-os_dbus-mqtt-pv):**
  measured total active power/lifetime yield, phase voltages/currents and grid
  frequency on `huawei_wechselrichter`. Optional phase-power estimates use the
  measured total power factor and are explicitly labelled; no individual phase
  power factors or phase energy counters are invented.
- Embedded MQTT broker with subscriber authentication and read-only topic
  access, or external broker publishing. TLS is supported in both modes.
- Web dashboard and YAML editor with configurable authentication, validation,
  backup/restore, atomic save and previous-configuration rollback.
- Read-only by default. Write mode supports standard register writes and the
  Huawei login exchange used by HA; inverter permissions still apply.
- Native Linux/systemd, Proxmox LXC and Docker deployment, structured JSON logs,
  health/readiness endpoints and automated failure tests.

## The Problem

The SDongle is sensitive to competing Modbus clients, request spacing and
response delays. Direct polling by several consumers can lead to slow responses
or timeouts. MQTT consumers also need physical units and a consistent payload
rather than raw Modbus register values.

## The Solution

```text
┌────────────────────┐          ┌─────────────────────────┐          ┌─────────────────────┐
│ Huawei SUN2000     │ ◄──────► │ Huawei Solar Smart Proxy│ ◄──────► │ Home Assistant      │
│ via SDongle        │  Modbus  │                         │  Modbus  │ wlcrs/huawei_solar  │
│ upstream port 502  │          │ Fast/Slow polling       │  :1502   │                     │
│ Huawei Smartmeter  │          │ SQLite register cache   │          └─────────────────────┘
└────────────────────┘          │ Web UI :8080            │
                                └────────────┬────────────┘
                                             │ MQTT JSON
                                             ▼
                                ┌─────────────────────────┐          ┌─────────────────────┐
                                │ Embedded broker :1883   │ ───────► │ Victron Venus OS    │
                                │ or external MQTT broker │          │ dbus-mqtt-pv        │
                                └─────────────────────────┘          └─────────────────────┘
```

The proxy continuously polls configured registers and serves fresh cached data
to Modbus clients. MQTT publishes the same inverter's PV data in the receiver's
`pv` JSON structure. HA and Victron are independent consumers; neither requires
the other. Register writes and uncached discovery reads share the same serialized
upstream connection. The SDongle's FusionSolar cloud connection is unchanged.

## Quick Start

The instructions below use the **SUN2000 M1 + one Huawei Smartmeter profile,
without a Huawei battery**. Adapt the inverter address, unit ID and register
groups to your installation. A battery connected to Victron does not require
Huawei battery polling.

### Linux / Proxmox LXC

Use a Debian-based Linux machine or an unprivileged Debian LXC with network
access to the SDongle. No USB passthrough is required. Run the installation
commands as root, or prefix administrative commands with `sudo`.
Go is normally not preinstalled in a minimal Debian/LXC image. The native build
requires **Go 1.27.4 or newer**; Debian's default `golang-go` package may be older.
Docker builds include their own Go toolchain.

```sh
apt-get update
apt-get install -y git ca-certificates
```

Check the installed version with `go version`. If Go is missing or too old,
follow the [official Go installation guide](https://go.dev/doc/install).

Clone, build and install the service:

```sh
git clone --branch main https://github.com/loewi1979/huawei-solar-smart-proxy.git
cd huawei-solar-smart-proxy
CGO_ENABLED=0 go build -trimpath -o huawei-solar-proxy .

useradd --system --home /var/lib/huawei-smart-proxy --shell /usr/sbin/nologin huawei-proxy
install -m 0755 huawei-solar-proxy /usr/local/bin/huawei-solar-proxy
install -d -o huawei-proxy -g huawei-proxy -m 0700 /etc/huawei-smart-proxy /var/lib/huawei-smart-proxy
install -o huawei-proxy -g huawei-proxy -m 0600 config.sun2000-m1.meter.example.yaml /etc/huawei-smart-proxy/config.yaml
install -m 0644 deploy/huawei-smart-proxy.service /etc/systemd/system/huawei-smart-proxy.service
```

Edit `/etc/huawei-smart-proxy/config.yaml` before starting:

1. Set `inverter.host` to the SDongle address and `inverter.port` to 502.
2. Set `cache_path` to `/var/lib/huawei-smart-proxy/cache.db`.
3. Bind the required listeners to the server's LAN address, or use `0.0.0.0`
   as in the [configuration example](#configuration).
4. Set a web token of at least 24 characters, or explicitly disable web
   authentication for your trusted LAN.
5. For Victron, enable MQTT and set broker credentials. For HA-only use,
   leave MQTT disabled. Both consumers can be enabled together.

```sh
runuser -u huawei-proxy -- /usr/local/bin/huawei-solar-proxy -config /etc/huawei-smart-proxy/config.yaml -check-config
systemctl daemon-reload
systemctl enable --now huawei-smart-proxy
systemctl status huawei-smart-proxy --no-pager
```

The service starts automatically after reboot. Open `http://PROXY_IP:8080/`
for the dashboard and `/config` for YAML management. Do not leave another
application polling the SDongle directly; point downstream consumers at the proxy.

### Docker Compose

The ready-to-run image is published on
[GitHub Packages](https://github.com/loewi1979/huawei-solar-smart-proxy/pkgs/container/huawei-solar-smart-proxy):

```text
ghcr.io/loewi1979/huawei-solar-smart-proxy:1.0.1
```

It supports **linux/amd64 and linux/arm64**; Docker selects the matching platform.
No Go installation, image build or registry login is needed. `1.0.1` pins the
application version; `latest` follows the newest stable image published here.

Download the Compose file and configuration example into a new directory:

```sh
mkdir -p runtime/config runtime/data
curl -fL https://raw.githubusercontent.com/loewi1979/huawei-solar-smart-proxy/main/compose.yaml -o compose.yaml
curl -fL https://raw.githubusercontent.com/loewi1979/huawei-solar-smart-proxy/v1.0.1/config.sun2000-m1.meter.example.yaml -o runtime/config/config.yaml
```

On an existing installation, keep your current YAML instead of downloading
the example over it. The supplied Compose service uses:

```yaml
image: ghcr.io/loewi1979/huawei-solar-smart-proxy:1.0.1
```

Edit `runtime/config/config.yaml` using the same settings as above. Inside
Docker, use `0.0.0.0` for listeners and
`/var/lib/huawei-smart-proxy/cache.db` for the cache path. For LAN consumers,
change the supplied `compose.yaml` port bindings from loopback to the host's
LAN address, for example replacing `PROXY_LAN_IP` below:

```yaml
ports:
  - "PROXY_LAN_IP:1502:1502"
  - "PROXY_LAN_IP:8080:8080"
  - "PROXY_LAN_IP:1883:1883" # Needed only for the embedded broker
```

```sh
sudo chown -R 10001:10001 runtime
sudo chmod 700 runtime/config runtime/data
sudo chmod 600 runtime/config/config.yaml
docker compose pull
docker compose run --rm proxy -config /etc/huawei-smart-proxy/config.yaml -check-config
docker compose up -d
docker compose logs -f proxy
```

The container runs as non-root and mounts the whole configuration directory so
backup/restore and atomic replacement work. To update later, select the desired
image version in `compose.yaml`, then run `docker compose pull` and
`docker compose up -d`. The existing configuration and SQLite data remain in
the mounted directories. The host port bindings stay loopback unless you
explicitly change them as shown above.

For a local source build, replace the service's `image` entry with
`build: {context: ., dockerfile: Dockerfile.standalone}` in a repository checkout
and use `docker compose build`. The preserved legacy HA add-on Dockerfile is a
separate path.

## Home Assistant Configuration

Install [wlcrs/huawei_solar](https://github.com/wlcrs/huawei_solar) and point it
at the proxy instead of the SDongle. For example, if your proxy is
`192.0.2.20` (illustrative address; replace it with your server's address):

| Setting | Value |
| --- | --- |
| Host | `192.0.2.20` |
| Port | **`1502`** |
| Slave IDs | **`1`** for the documented single-inverter profile |

Port 502 belongs to the upstream SDongle in this example; HA connects to the
proxy's `server.listen` port. Retain the appropriate inverter credentials if
using HA's configuration controls. Raw Modbus registers remain unchanged; HA
applies the register scaling, for example `2307` → `230.7 V`.

The M1 + Smartmeter profile includes metadata needed for device discovery.
`forward_unknown_reads: true` permits additional one-off feature/timezone probes
through the same upstream connection. HA may probe battery product identifiers
during discovery even when no Huawei battery is installed; there is no periodic
battery polling in this profile.

Set `read_only: false` and restart the proxy to enable configuration writes.
The following HA controls have been confirmed working by the operator against
a SUN2000-10KTL-M1:

- `switch.inverter_inverter_on_off`
- `number.inverter_power_derating_by_percentage`
- `number.inverter_power_derating`

The web dashboard also showed **Shutdown command** following shutdown.
Inverter authentication and permissions still apply. An acknowledged write is
mirrored in the cache; a subsequent hardware poll provides independent readback.

### Home Assistant polling frequency

The Huawei Solar integration polls the inverter **every 30 seconds by default**.
Connecting it to the proxy does not automatically change that interval.
Follow the integration's
[polling-frequency instructions](https://github.com/wlcrs/huawei_solar#inverter-polling-frequency)
to disable automatic polling in its system options and create custom
`homeassistant.update_entity` automations. Inverter and power-meter devices
require separate refresh automations; no Huawei battery automation is needed
for the profile documented here.

The dashboard suggests an HA polling interval for each fresh fast group:
**floor(acquisition median ÷ 2), with a minimum of 1 second**. For the measured
approximately 4.5s inverter/meter medians, the suggestion is **2 seconds** for
each device's refresh automation. This is a tuning heuristic, not a guaranteed
measurement rate or an automatically applied HA setting.

HA reads configured cached data; this does not accelerate upstream polling.
Some HA refreshes will repeat the same sample. A shorter HA interval reduces
the additional wait after a new sample reaches the cache: approximately 0–2s
with a 2s interval, instead of 0–5s with a 5s interval, plus request/HA processing
time. It cannot shorten a dongle timeout. The median is not a maximum latency.

The historical approximately 13s HA update interval in the comparison table
is an operator-recalled value for the previous installation, not the
integration's default interval.

## MQTT / Victron Configuration

The publisher supplies
[mr-manuel/venus-os_dbus-mqtt-pv](https://github.com/mr-manuel/venus-os_dbus-mqtt-pv)
on Victron Venus OS. Configure that receiver with:

| Setting | Value |
| --- | --- |
| Broker | Proxy address for embedded mode, otherwise your external broker |
| Port | `1883` for the illustrated embedded plaintext broker |
| Username / password | Your configured broker credentials |
| Topic | **`huawei_wechselrichter`** |

Use a **nonzero receiver timeout** appropriate for your installation. The stock
receiver does not consume the separate `/status` topic; with timeout zero it
can keep old values indefinitely after messages stop.

Example telemetry, with phase-power estimates enabled:

```json
{
  "pv": {
    "power": 1234,
    "energy_forward": 123.45,
    "L1": {"voltage": 230.7, "current": 1.801, "frequency": 50.01, "power": 411.33, "power_is_estimate": true},
    "L2": {"voltage": 231.2, "current": 1.79, "frequency": 50.01, "power": 409.60, "power_is_estimate": true},
    "L3": {"voltage": 229.8, "current": 1.82, "frequency": 50.01, "power": 413.84, "power_is_estimate": true}
  }
}
```

Values are illustrative. MQTT uses physical units: **W, kWh, V, A and Hz**.
`energy_forward` is lifetime inverter production. Smartmeter import/export
power is not substituted for PV power. Huawei supplies one measured grid
frequency, shared across phases.

With `mqtt.estimate_phase_power: true`, each phase estimate is
`voltage × current × measured total PF`, labelled `power_is_estimate: true`.
This assumes the same PF on all phases; estimates need not sum to measured
total power. The option defaults to false. Missing/stale optional fields are
omitted rather than filled with zero. Individual phase PF, phase energy and
ambiguous aggregate voltage/current are not fabricated.

The reviewed stock receiver requires phase power to process phase V/A/Hz,
ignores the estimate label in its display and can retain omitted fields.
The [receiver documentation](docs/VICTRON.md) includes a companion patch that
preserves measured diagnostics/labelled estimates and clears omitted fields.
Installing the proxy does not install that patch on a GX.

The embedded broker accepts MQTT Explorer's `#` and `$SYS/#` subscriptions,
but delivers only the configured telemetry and status topics. Remote publishing
is denied. Telemetry is non-retained QoS 0: leave subscribers connected to
receive live messages. To use an existing broker, set `mqtt.embedded: false`
and `mqtt.broker` to `tcp://HOST:1883` or certificate-validated `ssl://HOST:8883`.

## Configuration

The following is a complete M1 + Smartmeter configuration illustrating **both
outputs enabled**. Replace the documentation-only inverter address and password
placeholders before use. The public YAML example keeps MQTT disabled until you
configure credentials and uses loopback listeners initially.

```yaml
inverter:
  host: "192.0.2.10" # Replace with your SDongle address
  port: 502
  unit_ids: [1]
  timeout_ms: 15000
  wait_after_connect_ms: 1500
server:
  listen: "0.0.0.0:1502"
web:
  listen: "0.0.0.0:8080"
  auth_enabled: true
  token: "replace-with-a-random-token-of-at-least-24-characters"
polling:
  read_pause_ms: 1000
  fast_interval_s: 2
  slow_interval_s: 300
  fast_max_age_s: 30
  slow_max_age_s: 900
forward_unknown_reads: true # HA startup/feature probes
read_only: true             # Set false to enable inverter writes
log_level: info
cache_path: /var/lib/huawei-smart-proxy/cache.db
cache_ttl_h: 2
mqtt:
  enabled: true
  embedded: true
  listen: "0.0.0.0:1883"
  broker: "tcp://127.0.0.1:1883" # Used only with embedded: false
  username: pv_reader
  password: "replace-with-your-mqtt-password"
  topic: huawei_wechselrichter
  unit_id: 1
  interval_s: 2
  max_age_s: 30
  estimate_phase_power: false # Opt in to labelled V × I × total PF estimates
register_groups:
  - {name: fast_snapshot, address: 32000, count: 108, poll: fast, priority: 10}
  - {name: meter, address: 37100, count: 39, poll: fast}
  - {name: device_info, address: 30000, count: 65, poll: slow}
  - {name: pv_capabilities, address: 30071, count: 2, poll: slow}
  - {name: rated_power, address: 30073, count: 4, poll: slow}
  - {name: energy_statistics, address: 32108, count: 12, poll: slow}
  - {name: inverter_config, address: 40122, count: 10, poll: slow}
  - {name: grid_mppt_config, address: 42054, count: 4, poll: slow}
  - {name: power_control, address: 47415, count: 10, poll: slow}
```

For HA-only operation, use `mqtt.enabled: false`. For Victron-only operation,
no HA setup is required and unknown-read forwarding can remain disabled. The
Modbus listener still starts; it does not require a connected HA client.
Bind listeners to a specific interface if desired. Modbus has no built-in
authentication; keep services on your trusted network.

### Two-Tier Polling

| Tier | Profile contents | Configured wait / freshness limit |
| --- | --- | --- |
| Fast | Inverter status/alarms, PV string V/A, AC/DC power, phase V/A, total PF, frequency, diagnostics and lifetime yield; Huawei Smartmeter measurements | 2s / 30s |
| Slow | Identification/firmware, PV/MPPT counts, rated power, yield statistics including daily yield, and power/grid/MPPT settings | 300s / 900s |

The profile has nine groups and 254 raw registers. Diagnostic values remain in
contiguous fast reads to avoid extra requests. Daily yield is slow; lifetime
yield stays fast because the MQTT publisher requires it alongside active power.
See [polling groups and HA entity coverage](docs/POLLING_PROFILE.md).

Intervals are minimum waits after each group's completion, not guaranteed
measurement periods. Request pauses, other work and response delays add to the
actual interval. MQTT has a separate send interval and may repeat still-fresh
measurements. All groups are due at startup; listening starts before the first
scan completes. Slow requests can delay fast work while in flight.

### Defaults and customization

| Setting | Built-in default if omitted | M1 + Smartmeter example |
| --- | --- | --- |
| Fast / Slow wait | 5s / 300s | 2s / 300s |
| Request / connection pause | 500ms / 1000ms | 1000ms / 1500ms |
| Response timeout | 5000ms | 15000ms |
| Fast / Slow freshness limit | 30s / 900s | 30s / 900s |
| Cache retention | 2h | 2h |
| `read_only` | `true` | `true` |
| `forward_unknown_reads` | `false` | `true` |
| `web.auth_enabled` | `true` | `true` |
| `mqtt.enabled` / `mqtt.embedded` | `false` / `false` | `false` / `true` |
| MQTT interval / age limit | 5s / 30s | 2s / 30s |
| Phase-power estimates | Disabled | Disabled |

The host and register groups must be supplied. The built-in cache path is
`cache.db` relative to the working directory; the installation above explicitly
sets the absolute service path. Listener defaults are loopback on ports 1502,
8080 and 1883. The older split-group YAML example has a different register
selection; use `config.sun2000-m1.meter.example.yaml` for this workload.

- **No Huawei meter:** remove `meter` and adapt the consumers accordingly.
- **Huawei battery:** configure the appropriate supported register groups;
  none are included in this M1 + Smartmeter example.
- **Write controls:** set `read_only: false`; the legacy `allow_writes` alias
  cannot be combined with `read_only`.
- **Web login:** username `admin`, password `web.token`. With authentication
  enabled and an empty token, configuration management is disabled. Non-loopback
  binding requires a token of at least 24 characters. `web.auth_enabled: false`
  removes login from every web endpoint, including YAML configuration/backups;
  it does not disable MQTT authentication or change write policy.
- **TLS:** embedded MQTT uses `mqtt.tls_cert` and `mqtt.tls_key`; external
  `ssl://` connections validate the broker certificate. Use an HTTPS reverse
  proxy if encrypted web access is required.

## Web UI, Backup and Restore

The dashboard displays cached measurements and their ages. `/config` provides
YAML editing, validation, backup download, restore upload and rollback.

**Measured polling intervals** shows one row per unit/register group, including
the rolling median, last/longest gap, interval count and last successful update.
It keeps the last **100 successful acquisition-to-acquisition intervals** in
memory (a full window requires 101 successful acquisitions) and resets on service
restart. Unchanged measurements count; HA cache reads, acknowledged writes,
forwarded discovery reads and failed polls do not add observations. A gap caused
by failed polls/reconnects is included when the next successful poll is stored.
HA suggestions appear for fresh fast groups after five intervals; the window
continues filling to 100. Missing/stale groups have no current suggestion.
No additional requests are sent to the inverter for this calculation.

Saving validates and atomically replaces the configuration while retaining
`config.yaml.previous`. **Restart the service to activate saved settings.**

```sh
systemctl restart huawei-smart-proxy
```

For Docker, use `docker compose restart proxy`. Configuration backups contain
passwords and should be kept private. Offline rollback is also available:

```sh
systemctl stop huawei-smart-proxy
runuser -u huawei-proxy -- /usr/local/bin/huawei-solar-proxy -config /etc/huawei-smart-proxy/config.yaml -rollback-config
systemctl start huawei-smart-proxy
```

## Files and Logs

Paths for the systemd and Compose installations above:

| Item | Linux / LXC | Docker host → container |
| --- | --- | --- |
| Binary | `/usr/local/bin/huawei-solar-proxy` | Built into the image |
| YAML | `/etc/huawei-smart-proxy/config.yaml` | `./runtime/config/config.yaml` → same absolute path |
| Previous YAML | `/etc/huawei-smart-proxy/config.yaml.previous` | `./runtime/config/config.yaml.previous` → same absolute path |
| SQLite cache | `/var/lib/huawei-smart-proxy/cache.db` | `./runtime/data/cache.db` → same absolute path |
| Service unit | `/etc/systemd/system/huawei-smart-proxy.service` | Managed by `compose.yaml` |
| Application logs | systemd journal | Container stdout / Docker logs |

The application writes JSON to stdout, not to a separate logfile. Log storage
and rotation are managed by journald or Docker. Use `log_level: info` normally
and `debug` for detailed request timings.

```sh
# Linux / LXC: live log and recent JSON warnings/errors
journalctl -u huawei-smart-proxy -f
journalctl -u huawei-smart-proxy --since today --no-pager -o cat --grep '"level":"(WARN|ERROR)"'

# Docker
docker compose logs --tail=100 -f proxy
```

JSON `level` is not automatically mapped to journald priority; `-p warning`
alone can hide application warnings. The separate observer, if installed,
writes bounded `health.jsonl` rotations and `latest.json` to its configured
output directory; it is not required for normal operation.

## How It Works

- Successful group reads are stored with their acquisition timestamps in
  SQLite. Group timestamps are independent, not a simultaneous device snapshot.
- Cached multi-register reads check each register's own freshness limit,
  including requests crossing Fast/Slow groups. Missing/stale known ranges
  return Modbus exception 0x0B. Cache retention does not extend freshness.
- Unknown reads can be forwarded when enabled. Reads, writes and Huawei login
  traffic share the upstream connection and request spacing. Reconnects
  invalidate forwarded login sessions, requiring the consumer to authenticate
  again. Failed polls leave cached values and timestamps unchanged.
- MQTT requires fresh total power and lifetime yield. It stops telemetry when
  either is stale; the separate `/status` topic reports availability.
  Telemetry is non-retained QoS 0 without a persistent queue.
- `/healthz` checks HTTP availability. `/readyz` returns 503 when a configured
  fast group is missing/stale. Both follow the web authentication setting.

## Measured Performance and Dongle Versions

Baseline on **2026-11-18**: SUN2000-10KTL-M1, SDongleA-05, one Huawei Smartmeter,
no Huawei battery. The existing dongle is reported as **02312QMV-005** (label
confirmation pending), firmware **V200R022C10SPC320**. Measurements used the
documented 2s profile, a 1s request pause and a 15s response timeout.

| Measurement | Previous HA integration, direct SDongle connection | Existing dongle, HA connected | Replacement 02312QMV-004 |
| --- | ---: | ---: | --- |
| Typical polling interval | Approx. 13s, operator recollection | 4.49s, inverter acquisition median | — |
| Configured fast polling interval | Not recorded | 2s (`polling.fast_interval_s`) | — |
| Observation duration | — | 661s | Not yet measured |
| Successful cached Modbus reads / failures | N/A | 1,320 / 0 | — |
| MQTT messages / reconnects | — | 331 / 0 | — |
| MQTT send interval, median | — | 2.00s | — |
| Inverter acquisition, median / p95 / max | — | 4.49 / 8.00 / 23.05s | — |
| Meter acquisition, median / p95 / max | — | 4.48 / 8.02 / 19.03s | — |
| Highest observed fast cache age | N/A | 23s | — |

The approximately 13s historical reference is the operator's recollection of
HA-visible sensor updates, not a preserved acquisition benchmark or a verified
polling configuration. The proxy's typical interval is its inverter acquisition
median, distinct from its configured 2s minimum wait. Separate historical
inverter/meter timing distributions and matching
MQTT statistics were not recorded for that setup. The proxy columns measure
cache acquisition and MQTT publication; they do not measure HA's own sensor
update interval. Unavailable historical values are left blank rather than
inferred. The existing-dongle HA-connected column uses the proxy; the replacement
column is reserved for the same workload after the dongle swap.

In the HA-connected observation, all seven slow groups refreshed at least
twice, with complete intervals of approximately 302–316s. Cache-age sampling
was every 0.5s, so acquisition timings are approximate. Successful cached reads
do not mean error-free upstream polling: service logs confirmed intermittent
SDongle timeouts, including one during that observation. FusionSolar updates
and a complete production graph were confirmed by the operator.

The replacement **02312QMV-004** has been purchased; firmware and comparative
measurements will be added after installation. The
[evcc experience report](https://github.com/evcc-io/evcc/discussions/22534)
describes SDongleA-05 hardware differences and better Modbus performance with
this variant. Check exact part numbers and firmware when comparing devices;
those community results are separate from the measurements above.
See [measurement details](docs/POLLING_PROFILE.md) and
[polling tuning](docs/FAST_POLLING.md).

## Further Documentation

- [Deployment, configuration and rollback](docs/DEPLOYMENT.md)
- [Victron receiver behavior and companion patch](docs/VICTRON.md)
- [Polling profile, register ranges and HA entity coverage](docs/POLLING_PROFILE.md)
- [Fast polling measurements and tuning](docs/FAST_POLLING.md)
- [Security review](docs/SECURITY_REVIEW.md)
- [Upstream review](docs/UPSTREAM_REVIEW.md)
- [Preserved upstream README](docs/UPSTREAM_README.md)
- [Register map](REGISTERS.md)

## License

MIT. Original license and attribution are retained. See [LICENSE](LICENSE) and
[third-party notices](docs/THIRD_PARTY_NOTICES.md).

## Acknowledgements

A big thank you to the developers whose excellent work made this project possible:

- **Michael Siebert ([siebertm](https://github.com/siebertm))** for
  [huawei-proxy](https://github.com/siebertm/huawei-proxy), the foundation of this fork.
- **[mr-manuel](https://github.com/mr-manuel)** for
  [venus-os_dbus-mqtt-pv](https://github.com/mr-manuel/venus-os_dbus-mqtt-pv),
  which brings MQTT PV data into Victron Venus OS.

Thank you for sharing your work with the community and making it possible to build on it.
