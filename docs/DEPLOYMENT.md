# Standalone deployment and controlled test

No production Home Assistant, GX, inverter or FusionSolar configuration is
changed by building or running the local test suite. A separate SDongle Modbus
session may displace an existing reader: coordinate access before using a real
inverter address. Keep FusionSolar's cloud connection and existing settings.

## Native Linux / systemd

Build with Go 1.27.4 or newer, using the committed go.sum:

```sh
go test -race -count=1 ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -o huawei-solar-proxy .
sudo useradd --system --home /var/lib/huawei-smart-proxy --shell /usr/sbin/nologin huawei-proxy
sudo install -m 0755 huawei-solar-proxy /usr/local/bin/huawei-solar-proxy
sudo install -d -o huawei-proxy -g huawei-proxy -m 0700 /etc/huawei-smart-proxy /var/lib/huawei-smart-proxy
sudo install -o huawei-proxy -g huawei-proxy -m 0600 config.sun2000-m1.example.yaml /etc/huawei-smart-proxy/config.yaml
sudo install -m 0644 deploy/huawei-smart-proxy.service /etc/systemd/system/
```

Edit the YAML locally. Set `cache_path` to
`/var/lib/huawei-smart-proxy/cache.db`. Keep loopback listeners initially. Set
`web.token` to a random password of at least 24 characters; UI username is
`admin`. Keep MQTT disabled until the consumer/test broker is ready. Passwords
are part of the YAML and its backup; keep both private.

```sh
sudo -u huawei-proxy /usr/local/bin/huawei-solar-proxy -config /etc/huawei-smart-proxy/config.yaml -check-config
sudo systemctl daemon-reload
sudo systemctl enable --now huawei-smart-proxy
sudo journalctl -u huawei-smart-proxy -f
```

Service runs without Linux capabilities using port 1502, not privileged port
502. Do not broadly open firewall ports. After approval, bind the Modbus
listener to the test/LAN interface and permit only the intended client IPs.

Use an SSH tunnel for the local UI:

```sh
ssh -L 8080:127.0.0.1:8080 codex@TEST_CONTAINER_IP
```

Open `http://127.0.0.1:8080/` for status and `/config` for backup, YAML upload,
validation, save and rollback. Basic authentication is only private over an
SSH tunnel or HTTPS reverse proxy; direct HTTP exposes credentials in transit.
`/healthz` checks process HTTP availability; `/readyz` returns 503 when any fast
configured group is missing/stale. These endpoints share UI authentication.

Authentication defaults to `web.auth_enabled: true`. On an operator-approved
trusted LAN, `web.auth_enabled: false` explicitly disables login for status,
health, configuration editing, backups and rollback. Anyone able to reach that
listener can then read the YAML (including its passwords) and save settings for
the next restart. An existing `web.token` can remain for later re-enabling; it
is ignored while authentication is disabled. The custom request header, YAML
validation and body limits still apply. This option changes neither MQTT login
nor Modbus write policy.

## Logs and read-only soak monitoring

The application writes structured JSON to standard output. Under systemd the
service log is in the journal, not a file inside the project directory:

```sh
sudo journalctl -u huawei-smart-proxy -n 100 --no-pager
sudo journalctl -u huawei-smart-proxy --since today
sudo journalctl -u huawei-smart-proxy -f
```

`log_level: info` is the normal YAML setting; `debug` adds per-request timing
and is useful for a bounded polling investigation. It does not log raw register
values or passwords. Docker users use `docker compose logs`. Prefer the service
manager's rotation and retention over a second application logfile. Configure
journald persistence/space limits at the host level if needed; a YAML file-path
option is not required for systemd or Docker deployments.

The separate optional `deploy/observe_test.py` observer records availability,
read-only mode, restart count and a valid non-retained MQTT sample every minute.
It discards the measurement payload and makes no Modbus requests. Its private
JSON configuration supplies `web_token`, optional `web_url` (default
`http://127.0.0.1:8080`), `mqtt_probe`, `mqtt_credentials` and `output_directory`.
It writes `health.jsonl` (4 MiB with three rotated backups, about 16 MiB maximum)
and `latest.json` there. In the dedicated soak installation this directory is
`/home/codex/huawei-proxy/soak`. This is a local record, not an alert service.

## Installation without a Huawei battery

The optional contiguous profile reads inverter registers 32000–32107 and
device/rated-power information. It does not request Huawei storage groups.
An attached Huawei Smartmeter requires its own group, for example:

```yaml
register_groups:
  - {name: fast_snapshot, address: 32000, count: 108, poll: fast, priority: 10}
  - {name: meter, address: 37100, count: 39, poll: fast}
  - {name: device_info, address: 30000, count: 35, poll: slow}
  - {name: rated_power, address: 30073, count: 4, poll: slow}
```

Confirm meter status and firmware support during the read-only hardware test.
The meter is exposed through inverter unit 1 behind the SDongle; adding another
unit ID is not needed for this register block. A battery connected to Victron
does not justify Huawei battery polling. The SDongle is the transport gateway;
this profile does not claim to collect separate dongle management telemetry.
The additional meter request changes the load: the previously measured 2.59s
median applies to the inverter-only profile, not this combined profile.

For approved direct LAN testing, replace the three loopback listen addresses
with the container IP while keeping ports 1502 (Modbus), 8080 (web), and 1883
(embedded MQTT). Retain `read_only: true` and authentication. Also update the
observer's `web_url` and MQTT probe host if loopback is no longer listening.
No Home Assistant or Victron consumer configuration is changed by doing this.

## Backup, restore and rollback

Download YAML from `/config`. Restore/upload fills the editor; Validate does
not write. Save creates `config.yaml.previous` then replaces the active YAML
atomically using a same-directory temporary file. New settings activate on
explicit service restart, avoiding live mutation races. It does not change
Home Assistant or GX settings. Connectivity, port availability and filesystem
permissions cannot be proven by YAML validation alone.

After an unsuccessful restart, restore the previous configuration offline:

```sh
sudo systemctl stop huawei-smart-proxy
sudo -u huawei-proxy /usr/local/bin/huawei-solar-proxy -config /etc/huawei-smart-proxy/config.yaml -rollback-config
sudo systemctl start huawei-smart-proxy
```

The rollback command validates the previous file before replacing the current
one. Keep an additional off-device YAML backup. SQLite is a disposable cache,
not configuration or an authoritative energy ledger. If backing up SQLite,
stop the service or use SQLite's backup mechanism; do not copy a live WAL DB
without its journal. Read-time freshness still applies after restoring a DB.

## Docker

```sh
mkdir -p runtime/config runtime/data
cp config.sun2000-m1.example.yaml runtime/config/config.yaml
sudo chown -R 10001:10001 runtime
sudo chmod 700 runtime/config runtime/data
sudo chmod 600 runtime/config/config.yaml
```

In the container YAML, bind the services to `0.0.0.0:1502`, `0.0.0.0:8080`
and (when needed) `0.0.0.0:1883`. Set a >=24-character web token before binding
web to a non-loopback address. Set the absolute cache path shown above. The
Compose host bindings remain loopback, so ports are not exposed to the LAN.
Mount the whole config directory read/write to allow atomic rename and rollback;
a read-only single-file mount cannot support restore.

```sh
docker compose build
docker compose run --rm proxy -config /etc/huawei-smart-proxy/config.yaml -check-config
docker compose up -d
docker compose logs -f
```

Scratch runtime, non-root user, read-only root filesystem, no capabilities.
The provided image needs no Python, SQLite daemon or external MQTT broker.
With `mqtt.embedded: false`, set `mqtt.broker` to `tcp://host:1883` or
`ssl://host:8883`. TLS uses system CAs and validates certificates. For a private
CA, mount its CA bundle at `/etc/ssl/certs/ca-certificates.crt`. Embedded TLS
uses `tls_cert`/`tls_key`. External broker accounts need write access only to
the exact telemetry and status topics. Embedded subscribers can only read these
two topics and cannot publish. No broker persistence or telemetry retention.
MQTT Explorer may use its standard `#` and `$SYS/#` subscriptions. The broker
accepts those filters but checks each delivered topic too, so unrelated data
and broker-system topics remain unavailable. An exact subscription to
`huawei_wechselrichter` also works. Measurements are non-retained: leave the
client connected to receive the next live publication.

## Polling and failure policy

Due fast jobs precede slow jobs; within a tier, larger `priority` runs first,
with due time breaking ties. One register-group read is the non-preemptible
unit. A slow read can therefore delay a newly due fast read by one request
timeout plus the configured inter-read pause. Jobs are rescheduled from their
completion time; configured intervals are minimum waits, not fixed deadlines.
Overload can starve slow jobs; their values then become stale and are refused.

Each successful group is stored in one SQLite transaction with its actual
acquisition time. Independent groups can have different acquisition times;
there is no false claim of a simultaneous whole-device snapshot. Failed reads
do not touch timestamps; reconnect occurs before a later scheduled attempt.
Known stale Modbus ranges return exception 0x0B and are never forwarded. Unknown
reads are disabled by default; opt-in forwarding uses the serialized client and
can delay polling. `read_only: true` is the default and blocks register writes
and Huawei login commands at both the Modbus server and inverter-client layer.
Set `read_only: false` to opt into write forwarding, then restart the service.
The legacy `allow_writes: true/false` spelling remains supported with the inverse
meaning. Supplying both keys is rejected rather than choosing one silently.
The legacy HA options path remains read-only.

Read Device Identification (0x2B/0x0E) is forwarded in either mode. In write
mode, only Huawei login challenge/authentication subcommands 0x41/0x24 and
0x41/0x25 are forwarded, along with standard writes 0x06 and 0x10. Other private
commands remain blocked. HA owns the installer credentials and verifies the
challenge response; the proxy does not store or log those credentials. Huawei
permission errors (including 0x80) are preserved so HA can request login.

Login is bound to the upstream connection. If that connection is lost or
recreated, an authenticated downstream connection is closed on its next request,
forcing HA to reconnect and authenticate again. Use one trusted configuration
client; independent simultaneous installer sessions are not a supported isolation
model. Whether the installed HA/library/firmware combination can write a given
setting still needs an explicitly approved hardware write test. Enabling writes
does not bypass inverter permissions or authenticate without HA credentials.

`inverter.wait_after_connect_ms` supplies the quiet period after every new TCP
connection, including reconnects. The M1 example uses 1500 ms, a 1000 ms request
gap, and a 15000 ms response timeout as a conservative SDongle starting point.
Shorter response timeouts can turn delayed replies into transaction-ID errors.
Tune from measured reliability; a short smoke test is not an overnight soak.

Read-time age limits are independent of `cache_ttl_h`, which only limits disk
retention. A batched Modbus read crossing polling-group boundaries checks each
register against its own group's limit in one SQLite snapshot. A slow value's
longer age allowance never relaxes the freshness required for a fast value.
MQTT requires fresh power AND energy, rechecks freshness after a
broker connection delay, and stops telemetry on staleness. `/status` reports
online/stale/offline but the stock Victron driver does not consume this status.
See [VICTRON.md](VICTRON.md) before connecting a GX.

## Legacy HA add-on

Upstream packaging and JSON options remain in the fork for comparison. This
project's supported deployment path is standalone YAML. The legacy HA path has
no MQTT/config-edit controls and uses a loopback-only dashboard and disabled
writes. It must not be presented as a drop-in production add-on upgrade.

## Test rollout

1. Local simulated inverter and isolated broker: automated suite.
2. New Debian LXC: SSH key, native service, simulator, persistence/restart and
   offline tests. No production clients point to it.
3. Coordinate read-only dongle test; verify unit ID and supported registers,
   intervals, FusionSolar continuity and whether the existing reader conflicts.
4. Isolated GX receiver tests and aggregate-only patch review (VICTRON.md).
5. Review results and obtain explicit approval before production HA/GX changes.

The isolated Docker smoke test runs in GitHub CI. Native systemd deployment,
SDongle access and daytime HA/MQTT operation on Debian LXC have been exercised;
see the README and POLLING_PROFILE.md for measurements and operator-confirmed
write controls. Live GX/ESS behavior, Docker on the installation host and an
extended overnight hardware soak remain separate acceptance checks.
