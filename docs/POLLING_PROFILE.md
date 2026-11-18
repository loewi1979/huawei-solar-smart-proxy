# SUN2000 M1 + Huawei Smartmeter, without Huawei battery

Use `config.sun2000-m1.meter.example.yaml` for this workload. It defaults to
loopback, a simulated upstream, MQTT disabled and `read_only: true`. Replace
connection details for an approved installation. Private deployment settings
and Home Assistant registry exports must not be committed.

## Inventory and comparison

A read-only Home Assistant registry/source audit found 76 Huawei entities:
58 had been in use before the integration was disabled; 18 were already
disabled by the integration. The 58 comprise 57 readable entities and one
startup/shutdown switch. That switch reads device status and sends write-only
commands; its command registers must not be periodically read.

The revised profile covers all 57 previously used readable entities, plus 11
of the optional entities through the selected blocks. It does not create or
enable HA entities. Entity count is not raw register count: bitfields share
registers, 32-bit values need two registers, and contiguous blocks contain
unused addresses. The old four-group inverter/meter profile covered 186 raw
registers; this profile covers 222. No synthetic phase measurements are added.

Excluded optional HA entities: minimum/maximum PV voltage (32177–32178), four
per-MPPT energy counters (32212–32219), and backup/off-grid switching (47604).
They were already disabled, their usefulness/firmware support is unconfirmed,
and MPPT3/4 must not be assumed present on an M1 installation. Other-platform
HA helpers (Powercalc, Energy inverse power, utility meters) are derived values
and are not additional Huawei registers. No Huawei storage polling is included.

## Groups

| Group | Range | Minimum wait | Purpose |
| --- | --- | --- | --- |
| fast_snapshot | 32000–32107 | 2s | Inverter state/alarms, PV1/2 V/A, DC input W, AC W, phase V/A, peak power, PF, frequency, efficiency, temperature, insulation, startup/shutdown time, lifetime yield |
| meter | 37100–37138 | 2s | Grid import/export W and real phase powers, meter status, V/A, PF, frequency and energy counters |
| device_info | 30000–30034 | 300s | Model/serial/product information |
| rated_power | 30073–30076 | 300s | Rated and maximum active power |
| energy_statistics | 32108–32119 | 300s | DC input energy, statistics time, hourly/daily/monthly/yearly yield |
| inverter_config | 40122–40131 | 300s | Power factor/reactive settings and percentage/fixed-W derating |
| grid_mppt_config | 42054–42057 | 300s | MPPT scan enable/interval and grid adjustment settings |
| power_control | 47415–47424 | 300s | Active power control, export limits, limitation mode, adjustment/protection parameters and fail-safe threshold |

The fast snapshot has priority 10; the meter is also fast and precedes slow
work. Diagnostic V/A and meter energy remain fast because splitting them out
would add requests and overhead to the already tested two-block arrangement.
The lifetime inverter yield remains fast because MQTT requires it fresh along
with active power. Daily yield is an independent slow value and uses the slow
900s freshness ceiling. Battery fields are omitted from the dashboard when
their ranges are not configured.

All configured groups are due at startup; fast work takes precedence. The
service does not block listening until an initial scan has completed. Slow
jobs must make progress under load; a 2s configured wait is not a guaranteed
2s acquisition interval. Keep the 1s request gap, 1.5s connect quiet period
and 15s timeout. The additional four slow groups add four requests per five
minutes and additional startup work. Validate their support and timing on the
actual firmware. Unsupported optional registers must not cause repeated
reconnects to be mistaken for a healthy profile.

## Write tests

`read_only: false` explicitly enables forwarding of standard writes and the
supported Huawei login exchange. HA owns the installer credentials and the
inverter still enforces permissions. It does not automatically enable HA,
retarget HA/GX, or perform any inverter write. Startup/shutdown commands are
40200/40201; they are write-only, with state read from 32089. MPPT and power
configuration ranges above support cached reads of their settings.

A successful Modbus write acknowledgement is not an independent hardware
readback: the proxy mirrors the acknowledged value in its cache. The slow poll
reads the device again within the configured five-minute schedule, subject to
request delays. Check that independent refresh before treating a parameter as
persisted. Real hardware writes and installed HA authentication compatibility
remain acceptance tests until explicitly exercised.

Reference definitions were checked against the installed Huawei HA component
and the source definitions in [huawei-solar-lib](https://github.com/wlcrs/huawei-solar-lib/tree/aaefb69f2d985826bf34603f341cd3ad3bd296f8).
