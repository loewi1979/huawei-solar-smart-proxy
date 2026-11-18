# Security and verification review — 2026-11-17

Development review, not an independent penetration test or a production approval.

## Release 1.0 checks — 2026-11-18

- Gitleaks 8.30.1 reported no leaks in the project directory or all reachable
  Git history before release. Scanner output was redacted; reports remain local.
- A separate check found no known private deployment credentials, SSH private
  keys, household addresses, device serial or private access-file references
  in the release file set. Example placeholders and isolated test credentials
  are not installation credentials. Private access files stay outside the repo.
- The complete local Go test suite and vet passed. Govulncheck v1.9.0 reported
  no vulnerabilities for the scanned target on this date.
- Actionlint validated the test and release workflows. Release packaging uses
  `git archive`, so untracked local configuration and tool/access directories
  cannot enter the published source/binary bundles.
- The original MIT copyright remains; the fork adds the requested 2026 Jürgen
  Löw attribution. The optional Victron patch's upstream MIT notice is retained
  in THIRD_PARTY_NOTICES.md.

These are point-in-time checks, not a guarantee against every possible secret
or future vulnerability. Linux race, Docker and receiver checks run in CI.

## Changes made

- Read-time freshness, missing/future timestamp rejection, per-unit isolation,
  strict ranges and stale Modbus exception. Old persisted values do not become
  current merely because the service restarted or a poll failed.
- Standalone listeners default to loopback and port 1502. Writes and unknown
  reads default off. Writes remain an explicit capability, protected by network
  isolation, not application authentication.
- `read_only` is enforced in both server and inverter client. Legacy
  `allow_writes` is accepted only without `read_only`; ambiguous or null policies
  are rejected. Huawei 0x41 login commands require write mode and a strict
  subcommand/shape allowlist. No arbitrary private-command passthrough exists.
  Device identification 0x2B/0x0E remains a read-only forwarded operation.
- Huawei credentials/challenge responses are forwarded without logging or
  storing them. Upstream reconnects invalidate authenticated downstream sessions.
  One trusted configuration client is supported; multi-user login isolation is
  not claimed. Permission-denied exceptions are preserved rather than hidden
  behind generic device failures.
- Maximum 64 simultaneous Modbus clients, 30-second socket deadlines and
  cancellation-driven connection closure. Strict MBAP lengths, range overflow
  and write count checks. Forwarded requests share the serialized device client.
- With the default `web.auth_enabled: true`, non-loopback web binding requires
  a >=24-character token. Explicit `false` is an operator opt-out for trusted
  LANs and exposes every web endpoint, including YAML secrets and configuration
  mutations, without login. All web endpoints retain no-store, frame denial and content-type hardening.
  Config mutations require POST and a custom header; no CORS permission is
  provided. Body size is limited to 1 MiB and HTTP timeouts are bounded.
- Strict YAML fields, exactly one YAML document, range/overlap/unit validation,
  interval bounds and atomic same-directory configuration replacement. Backup
  and previous files contain secrets and must remain private (0600).
- Embedded broker uses exact credential comparisons, denies remote publishing,
  and grants read access only to the exact data/status topics. Limits: 64 clients,
  64 KiB packets, no persisted sessions. TLS is optional. Remote publishing is
  only performed by the inline client; passwords never enter logs.
- External publisher has bounded connection/write waits, no persistent session,
  no stored telemetry queue, non-retained QoS 0 measurements, retained-data
  cleanup on connect, and freshness revalidation after connecting. TLS verifies
  server certificates; it does not offer an insecure skip-validation switch.
- Non-root systemd/Docker examples, minimal container image, no capabilities,
  read-only container filesystem except explicit config/data volumes.
- Go toolchain baseline 1.27.4. Updated golang.org/x/net and x/sys to corrected
  releases after the first scan showed non-reachable dependency advisories.

## Verification performed locally

Go 1.27.4 on Windows: compilation, go vet, tests with temporary SQLite and
loopback sockets. Linux amd64 and arm64 builds with CGO disabled. Example YAML
validation. Automated tests cover strict config validation, cache persistence,
stale/future/missing samples, unit isolation, Modbus exceptions and write
protection, measured MQTT mapping, invalid register sentinel, backup/restore/
rollback, authentication/CSRF header, TCP simulator reconnect, embedded broker
authentication and stale silence, and external broker publishing.

GitHub Linux race tests, Linux vulnerability scanning, Docker image build and
an isolated non-root container smoke test have also passed. The container test
checks offline startup, authenticated UI endpoints, missing-data readiness,
backup, rejected invalid restore, valid restore, rollback and SIGTERM shutdown.
Reconnect attempts preserve the minimum request gap, including on failures.

Additional emulator tests cover read-only defaults/legacy migration, both
register write functions, low-level write denial before connecting, unchanged
permission exceptions, device identification, allowed/blocked Huawei login
frames, reconnect invalidation and connection quiet periods. Login frame shapes
were checked against huawei-solar-lib commit
aaefb69f2d985826bf34603f341cd3ad3bd296f8; real installer credentials were not used.
Source: https://github.com/wlcrs/huawei-solar-lib/blob/aaefb69f2d985826bf34603f341cd3ad3bd296f8/src/huawei_solar/modbus_pdu.py

A coordinated native Debian LXC read-only smoke test confirmed the model,
cached Modbus responses, local MQTT telemetry and readiness behavior. A separate
persisted-cache test with an unavailable loopback inverter confirmed stale fast
register rejection, readiness 503 and MQTT silence while the broker remained
alive. Raw measurements, IP addresses and credentials are intentionally excluded
from this public review. The test processes were stopped afterwards. No hardware
write, installer login or FusionSolar setting change was performed.

Official govulncheck v1.9.0 after dependency updates: **No vulnerabilities found**
for the scanned Windows target on review date. This is a point-in-time database
result, not a guarantee. Linux race/vulnerability and Docker runtime jobs are
defined in CI; their latest result is visible on the development pull request.

The real Victron driver's on_message and _update functions were extracted and
executed with isolated globals and a fake D-Bus dictionary. Tests verify timeout
exit, stale hold with timeout=0, aggregate-to-L1 fallback, separation of status
and data topics, and the optional aggregate-only companion patch. No real D-Bus,
GX device or ESS was involved.

## Remaining limits / rollout gates

- Modbus is plaintext and unauthenticated. Keep it on a trusted, firewalled LAN;
  never expose it publicly. Enabling unknown reads/writes allows trusted clients
  to consume inverter bandwidth or change settings. Read-only is recommended.
- HTTP Basic authentication is not encryption. Keep UI loopback through SSH
  or place it behind a trusted HTTPS reverse proxy. Embedded MQTT plaintext
  credentials need an equally trusted network or TLS. External broker ACLs and
  TLS credentials remain the operator's responsibility.
- Config validation proves syntax and bounds, not connection success, port
  availability, supported firmware registers, filesystem access or end-to-end
  rollback after power loss. Atomic rename does not guarantee directory metadata
  survives every filesystem/power-loss scenario; keep off-device backups.
- Register groups are independently acquired, not a simultaneous whole-device
  snapshot. SQLite batching keeps multiword values atomic. System-clock jumps
  backward reject future data; forward jumps can mark samples stale.
- Slow requests can delay fast jobs by one request plus enforced pause. Unknown
  forwarded reads/writes can also contend. Polling cannot promise an interval
  shorter than dongle throughput. Hardware measurements determine final tuning.
- The stock GX driver can present aggregate energy as L1 energy and keep old
  values until timeout. The companion patch is not yet validated on GX/ESS.
  See VICTRON.md; this is a required production acceptance item.
- HA legacy packaging is retained for provenance, not validated for production
  upgrade. Linux service/container runtime tests and an overnight hardware soak
  remain pending. No production HA/GX/inverter settings have been changed.
