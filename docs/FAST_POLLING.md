# Fast polling: configuration is not the measured refresh period

`fast_interval_s` is the minimum wait after a group finishes, not a deadline
for a complete scan. The client serializes requests, enforces a minimum pause
after each attempt, and allows only one upstream connection. Four fast groups
cannot all be updated once per second just by setting this value to 1.

## Optional contiguous-read profile

`config.sun2000-m1.fast.example.yaml` replaces the four fast reads with one
holding-register read from 32000 through 32107 (108 registers, below the Modbus
125-register limit). This covers the existing state, PV, grid-power and total
energy fields in one acquired snapshot. Reserved/unused words can be present;
they are raw device responses and are not turned into invented measurements.
The MQTT publisher still emits only measured aggregate power and total energy.

This range was accepted in a coordinated read-only SUN2000-10KTL-M1 / SDongleA-05
test. Support is firmware-specific; some devices can reject ranges containing
reserved registers. The original split-group example remains available. Do not
replace its four groups with a shorter interval alone: the fast priority queue
can then keep all slow groups from being scheduled, even without device errors.

The optional profile starts at:

```yaml
inverter:
  timeout_ms: 15000
  wait_after_connect_ms: 1500
polling:
  read_pause_ms: 1000
  fast_interval_s: 2
  slow_interval_s: 300
  fast_max_age_s: 30
  slow_max_age_s: 900
read_only: true
```

Use the complete example; the fragment above omits host, unit, listeners and
register groups. The optional example also sets `mqtt.interval_s: 2`; publishing
has its own interval and does not automatically accelerate when polling changes.

## Interpreting a benchmark

### Observed comparison (nighttime, 2026-11-17)

All runs kept a 1000ms request pause, 1500ms connection quiet period, 15000ms
response timeout and 300s slow interval. No extra HA/battery/meter load was added.
The figures below describe cache acquisition intervals, not the inverter's
internal measurement clock.

| Profile | Configured fast wait | Test duration | Median power acquisition interval | Result |
| --- | ---: | ---: | ---: | --- |
| Four split fast groups | 5s | 361s | 7.12s | 197 observed requests, no polling errors, slow groups refreshed |
| Four split fast groups | 4s | 34s, stopped | about 6.5s | No device errors, but both slow groups remained unpolled; rejected as a scheduler-starvation profile |
| One contiguous 108-register group | 3s | 90s | 3.86s | No polling errors; one 4.93s reply. Small-sample p95 was inconclusive, not a proven overload |
| One contiguous 108-register group | 2s | 360s follow-up | 2.59s | 124 observed requests, no polling errors, both slow groups refreshed |
| One contiguous 108-register group | 1s | 342s, stopped | 2.07s before failure | One request hit the 15s timeout; not recommended |

The 2s follow-up had a p95 acquisition interval of 5.24s and a maximum interval
of 6.29s. Its median response latency was 586ms. A preceding 120s comparison at
2s also passed without errors (median acquisition interval 2.68s). MQTT/cache
checks confirmed the known aggregate fields remained valid in the contiguous
range. The longer 1s test failed despite a successful 120s initial test: short
tests alone were insufficient to select it.

**Provisional recommendation: the optional 2s contiguous profile, keeping the
1s request pause.** This is not a universal firmware limit or proof that load
alone caused the 1s timeout. Validate during changing PV output, with the final
HA/GX and battery/meter workload, and independently confirm FusionSolar updates.
The tests were read-only; all test services were stopped afterwards. Private
addresses, credentials, raw logs and household measurements are not published.

### Reproducing the procedure

`scripts/benchmark_fast_poll.py` uses the real proxy and observes only acquisition
timestamps in its SQLite cache. Debug-level client logs provide response timing
without register values. Each profile uses a separate cache and conservative
connection/request delays. Profiles stop on polling errors, missing initial
groups, or fast data exceeding a 20-second age margin. The soak checks that
slow groups are refreshed after their 300-second interval too.

An occasional multi-second response also occurs with the reference profile.
One outlier among fewer than 20 samples can become the empirical 95th percentile;
that is not by itself proof of sustained overload. The harness reports median,
p95, maximum, errors, acquisition gaps and slow-group progress separately. Its
comparison limits are test criteria, not a Huawei specification or a guarantee.

Example on an already coordinated Linux test host:

```sh
python3 scripts/benchmark_fast_poll.py ./huawei-solar-proxy private-read-only.yaml ./benchmark-results
# Explicit follow-up for a selected profile:
python3 scripts/benchmark_fast_poll.py ./huawei-solar-proxy private-read-only.yaml ./contiguous-results \
  --profile contiguous --intervals 2 1 --seconds 120 --soak-seconds 360
# Validate one candidate directly, after a recovery pause:
python3 scripts/benchmark_fast_poll.py ./huawei-solar-proxy private-read-only.yaml ./candidate-2s \
  --profile contiguous --intervals 2 --soak-only --soak-seconds 360
```

The base YAML must retain read-only mode, a 1000ms request gap, a 1500ms
connection quiet period, a 15s response timeout, unit 1 and the tested local
listeners. Do not run concurrently with another direct dongle reader. Output
directories are private because configuration/log files can contain addresses
and credentials. Do not commit raw results from a household installation.

These are short nighttime tests, not daytime load or overnight acceptance.
They cannot establish the inverter's internal measurement update cadence while
power is constant. Additional battery/meter groups, forwarded HA requests and
production GX/HA traffic need a new combined-load test. FusionSolar continuity
must be independently confirmed; no FusionSolar settings are changed here.
