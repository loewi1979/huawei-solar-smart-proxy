# Security and verification review — 2026-11-17

Development review, not an independent penetration test or a production approval.

## Changes made

- Read-time freshness, missing/future timestamp rejection, per-unit isolation,
  strict ranges and stale Modbus exception. Old persisted values do not become
  current merely because the service restarted or a poll failed.
- Standalone listeners default to loopback and port 1502. Writes and unknown
  reads default off. Writes remain an explicit capability, protected by network
  isolation, not application authentication.
- Maximum 64 simultaneous Modbus clients, 30-second socket deadlines and
  cancellation-driven connection closure. Strict MBAP lengths, range overflow
  and write count checks. Forwarded requests share the serialized device client.
- Non-loopback web binding requires a >=24-character token. All web endpoints
  use the same authentication, no-store, frame denial and content-type hardening.
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

Official govulncheck v1.9.0 after dependency updates: **No vulnerabilities found**
for the scanned Windows target on review date. This is a point-in-time database
result, not a guarantee. Linux race/vulnerability and Docker build jobs are
defined in CI and must be checked there or on the Linux test host.

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
