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
registers; the current profile covers 254 including startup metadata. No synthetic phase measurements are added (optional phase-power estimates are explicitly labelled).

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
| device_info | 30000–30064 | 300s | Model, serial, product, firmware and software strings; covers HA's combined 30015/50 startup read |
| pv_capabilities | 30071–30072 | 300s | PV string count and MPPT count required during HA device creation |
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

### Home Assistant device discovery

Entity inventory alone does not cover startup dependencies: the installed HA
library also reads serial number, product number, firmware and software as one
30015/50 request, then reads PV string count at 30071. The earlier short metadata
block was insufficient and caused HA to report a generic connection failure
despite a working TCP connection. Those mandatory reads are now cached.

This HA-oriented profile also enables `forward_unknown_reads` for the library's
on-demand feature masks and timezone/DST reads. These use the same serialized
upstream connection and request spacing; no second dongle connection is opened.
They are not added to the periodic polling list. Unsupported hardware feature
probes can therefore return actual device errors for HA's discovery logic to
handle without causing a failing periodic poll every five minutes. The library
can also probe storage product identifiers once during discovery, even when no
Huawei battery is installed; no battery telemetry is periodically polled here.
Known stale cached ranges remain rejected, and write permission still follows
`read_only`. On-demand reads add real upstream work and must be included in the
final HA-connected cadence test. For a publisher-only installation without HA,
unknown-read forwarding can remain disabled.

### Hardware check on 2026-11-18

With the approved LAN profile active, a 330s daytime cache/readiness check
completed 165 reads of each of the eight groups without a failed response.
Daily yield was valid, status 0x0200 decoded as On-grid, and unconfigured battery
widgets were absent. A separate 330s observation of dashboard acquisition ages
confirmed at least one new acquisition for every slow group. Observed maximum
ages were 301–320s, illustrating the scheduling overhead beyond the 300s wait.
MQTT remained valid and no automatic service restart was recorded. This checks
cache availability and refresh progress, not every individual upstream attempt.

An additional combined read of 32106–32119 exposed a freshness boundary bug:
individual fast and slow reads passed, but the combined response was incorrectly
limited to 30s for all registers. The regression test exercises a real Modbus
connection with three-minute-old slow data and fresh fast data. The corrected
server applies each register's own group age limit within one SQLite snapshot;
stale fast data is still rejected after 30s and stale slow data after 900s.
The correction was subsequently deployed and the combined read was included in
the successful hardware checks below.

### Cadence with Home Assistant connected, 2026-11-18

A separate 661s daytime observation with HA connected completed 1,320 cached
Modbus reads, including combined 32106/14 reads, without a failed response.
All seven slow groups refreshed at least twice. MQTT delivered 331 valid
messages with all three phase diagnostics and explicitly labelled phase-power
estimates; there were no MQTT reconnects. This test made no inverter writes.

| Observation | Median | 95th percentile | Maximum |
| --- | --- | --- | --- |
| MQTT message interval | 2.000s | 2.000s | 2.016s |
| Inverter fast-block acquisition interval | 4.493s | 8.000s | 23.047s |
| Meter-block acquisition interval | 4.484s | 8.016s | 19.031s |

Cache acquisitions were inferred from age resets sampled every 0.5s; their
timings are approximate. Slow-group intervals were 301.8–315.9s, with only
one or two complete intervals observed per group. The highest observed fast
cache age was 23s, below the configured 30s ceiling. Of the 331 MQTT messages,
199 repeated the previous active-power value: message cadence is not fresh
measurement cadence.

A preceding 721s observation before HA connected completed 1,440 cached reads
and 361 MQTT messages without downstream errors or MQTT reconnects. Fast-block
medians were approximately 4.5s; maximum inverter/meter intervals were 20.0s
and 25.0s. The HA-connected test therefore did not show a clear deterioration,
but these bounded observations do not establish long-term reliability.
Successful cached reads do not prove that every upstream poll succeeded or
explain the long acquisition gaps. A subsequently supplied service log confirms
six upstream TCP read timeouts between 13:42 and 14:26 local time, including one
inverter fast-block timeout during the HA-connected observation. Affected groups
were the inverter fast block, meter and slow power-control block. The observed
cache availability therefore must not be described as error-free upstream
polling. Several warning intervals were approximately 330s; this suggests a
periodic cause but does not identify it. FusionSolar upload contention is an
unverified hypothesis, not an established cause. Journald's priority filter
alone does not select embedded JSON `level` fields.

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
