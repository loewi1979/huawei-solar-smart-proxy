# Victron receiver compatibility and failure handling

Reviewed and exercised: `mr-manuel/venus-os_dbus-mqtt-pv` commit
`033663ed622c2d7959cde27a03a014463ca1227c`.

Source: https://github.com/mr-manuel/venus-os_dbus-mqtt-pv/blob/033663ed622c2d7959cde27a03a014463ca1227c/dbus-mqtt-pv/dbus-mqtt-pv.py

## Wire contract

Topic: `huawei_wechselrichter`, non-retained QoS 0 telemetry, clean sessions.

```json
{"pv":{"power":1234,"energy_forward":123.45,"L1":{"voltage":230.7,"current":1.801,"frequency":50.01},"L2":{"voltage":231.2,"current":1.79,"frequency":50.01},"L3":{"voltage":229.8,"current":1.82,"frequency":50.01}}}
```

`power` is measured signed total active power (holding registers 32080–32081,
W). `energy_forward` is measured accumulated yield (32106–32107, raw / 100
kWh). Both samples must be present and fresh. The publisher does not derive
phase powers from voltage/current or the total power factor, split energy
between phases, or publish a fabricated zero when data are unavailable.

Available optional AC diagnostics are published independently under L1/L2/L3:

| Field | Inverter registers | Scale / meaning |
| --- | --- | --- |
| L1/L2/L3 voltage | 32069 / 32070 / 32071 | unsigned U16 / 10, V phase-to-neutral |
| L1/L2/L3 current | 32072–32073 / 32074–32075 / 32076–32077 | signed I32 / 1000, A |
| L1/L2/L3 frequency | 32085 | unsigned U16 / 100, Hz; one measured grid frequency shared across phases |

Only present, fresh, non-sentinel fields are included; measured zero is valid.
Missing/stale optional data are omitted without suppressing fresh required
totals. The unambiguous single aggregate `pv.voltage` / `pv.current` fields are
not available for this M1, so no averaging, summing or L1 substitution is used.
Per-phase active power, PF and lifetime energy are absent because dedicated
inverter measurements are not available. Smartmeter phase power is grid
import/export and must not be relabelled as inverter PV phase power.

The generic upstream register map is retained; M1 firmware support must still
be checked against the actual device. PV input power must not be confused with
AC active power or grid-meter import/export.

## Receiver caveat: aggregate values become L1 in the stock driver

Stock `_update` copies total power, current, voltage and energy to L1 when no
phase power is supplied. Stock `on_message` derives current from power / its
configured default voltage when current is missing. That is receiver behavior,
not a measurement made by this proxy. Unmodified stock behavior does **not**
satisfy a requirement that all displayed phase energy and current be measured.
The reviewed stock parser also requires `Lx.power` before reading any of that
phase's voltage/current/frequency. Supplying measured diagnostics alone does
not make the stock receiver use them; do not fabricate phase power as a workaround.

`victron-aggregate-only.patch` is a separate, opt-in companion patch, prepared
against the exact commit above. With `[PV] aggregate_only = 1`, it leaves total
power and total energy available, clears unmeasured aggregate voltage/current
and phase power/PF/energy paths to `None`, and preserves optional measured
phase voltage/current/frequency independently of phase power. Omitted optional
fields are cleared on the next valid PV message so old phase readings do not
remain displayed. It does not change the timeout.
The patch has been exercised with the driver's actual message/update functions
using a fake D-Bus dictionary. It is **not deployed** and is **not yet tested
on Venus OS/ESS**. Whether the GX UI and ESS accept this aggregate-only device
is an explicit rollout gate. Do not represent aggregate values as true L1 data.

Reproduce without installing D-Bus or changing GX:

```sh
git clone https://github.com/mr-manuel/venus-os_dbus-mqtt-pv.git /tmp/venus-mqtt-pv
git -C /tmp/venus-mqtt-pv checkout 033663ed622c2d7959cde27a03a014463ca1227c
python3 scripts/check_victron_driver.py /tmp/venus-mqtt-pv/dbus-mqtt-pv/dbus-mqtt-pv.py
python3 scripts/prepare_victron_patch.py /tmp/venus-mqtt-pv/dbus-mqtt-pv/dbus-mqtt-pv.py /tmp/aggregate.patch
python3 scripts/check_victron_driver.py /tmp/aggregate.patch.test-driver.py --aggregate-only
```

## Silence is a timeout, not an immediate disconnect indication

- Driver timeout defaults to 60 seconds; `timeout = 0` disables it and preserves
  stale D-Bus values indefinitely. Never disable it for this integration.
- `_update` checks once per second and calls `sys.exit()` when
  `now - last_changed > timeout`. It does not immediately clear power or set
  `/Connected = 0` when the publisher stops.
- The process exit should release the D-Bus service; supervision/restart and
  actual ESS response must be tested on GX. There is no claim that a zero is
  published or that ESS responds instantaneously.
- The receiver's MQTT disconnect callback does not itself invalidate D-Bus
  measurements. It tries to reconnect. Its shared `connected` flag and callback
  behavior deserve an actual broker-loss test with the installed Paho version.
- Each valid JSON message updates `last_changed` before validating the `pv`
  measurement. Empty JSON objects/heartbeats on the data topic can therefore
  mask stale data. This proxy sends status only to the separate `/status` topic.
- The receiver ignores a sample timestamp; retained/queued old messages can
  masquerade as new. This proxy uses QoS 0 with no persisted telemetry queue,
  non-retained measurements, and clears an existing retained data message after
  every external-broker connection. Provision the broker/ACL so no other
  publisher can write to the data topic. Clear any pre-existing retained data
  **before** starting the GX subscriber; a proxy cannot prevent an earlier
  subscriber from receiving pre-existing retained data.
- With the defaults, last measured values can remain visible for approximately
  `proxy max_age_s (30) + driver timeout (60) + scheduling delay` after an
  acquisition failure. Set an explicit nonzero receiver timeout (e.g. 15 seconds)
  only after assessing the system's acceptable loss window. Fast polling is
  best effort; timeouts and dongle limits can exceed the configured interval.

## Candidate GX configuration — do not apply before approval

```ini
[DEFAULT]
device_name = Huawei SUN2000-10KTL-M1
device_instance = 100
timeout = 15
voltage = 230
frequency = 50

[PV]
max = 10000
position = 0
standby_power = 0
aggregate_only = 1

[MQTT]
broker_address = TEST_BROKER_ADDRESS
broker_port = 1883
username = gx
password = REPLACE_LOCALLY
topic = huawei_wechselrichter
```

Confirm device instance uniqueness and AC position from actual wiring. The
`aggregate_only` option only has an effect with the reviewed companion patch.
Use TLS when the network is not fully trusted. Keep certificate validation on.

## Hardware acceptance

Record installed GX driver SHA, Venus OS and Paho versions, inverter and
SDongle firmware. On an isolated/test GX, verify normal data, constant values,
publisher stop, proxy kill, broker stop, broker restart, dongle disconnect,
driver restart, retained-message removal and recovery. Inspect `/Ac/Power`,
`/Ac/Energy/Forward`, phase paths, `/Connected`, process supervision, VRM and ESS
behavior. Approve production HA/GX changes separately after reviewing results.
