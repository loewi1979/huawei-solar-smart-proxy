"""Coordinated read-only SDongle benchmark using the real proxy and SQLite cache.

Runs split-group and optional contiguous-block profiles, stopping a profile at
the first polling error or starvation. Private YAML/logs stay in a 0700 output
directory. This is a short smoke/soak comparison, not a hardware safety guarantee.
"""
import argparse
import datetime as dt
import json
import math
import pathlib
import re
import signal
import socket
import sqlite3
import statistics
import subprocess
import time

FIELDS = {"power": 32064, "energy": 32106, "state": 32000, "pv": 32016}
SLOW = {"device_info": 30000, "rated_power": 30073}


def quantile(values, fraction):
    return sorted(values)[max(0, math.ceil(len(values) * fraction) - 1)] if values else None


def replace_scalar(text, key, value):
    changed, count = re.subn(r"(?m)^(\s*" + re.escape(key) + r":)[^\n]*$", lambda m: m[1] + " " + str(value), text)
    if count != 1:
        raise ValueError("Expected exactly one configuration field: " + key)
    return changed


def run_stage(args, base, out, profile, interval, seconds, latency_limit, label):
    directory = out / label
    directory.mkdir(mode=0o700)
    cache = directory / "cache.db"
    config = replace_scalar(base, "fast_interval_s", interval)
    config = replace_scalar(config, "cache_path", json.dumps(str(cache)))
    config = replace_scalar(config, "log_level", "debug")
    if profile == "contiguous":
        config = config.split("register_groups:", 1)[0] + """register_groups:
  - {name: fast_snapshot, address: 32000, count: 108, poll: fast, priority: 10}
  - {name: device_info, address: 30000, count: 35, poll: slow}
  - {name: rated_power, address: 30073, count: 4, poll: slow}
"""
    path = directory / "config.yaml"
    path.write_text(config)
    path.chmod(0o600)
    log_path = directory / "proxy.log"
    log_path.touch(mode=0o600)
    log_output = log_path.open("w")
    log_input = log_path.open()
    process = subprocess.Popen([str(args.binary), "-config", str(path)], cwd=directory, stdout=log_output, stderr=log_output)
    started = time.monotonic()
    warmup_until = dt.datetime.now(dt.timezone.utc).timestamp() + 15
    updates = {key: [] for key in FIELDS | SLOW}
    observed = {}
    max_age = {key: 0.0 for key in FIELDS}
    errors = []
    latencies = []
    requests = 0
    reason = None
    db = None
    try:
        while time.monotonic() - started < seconds:
            elapsed = time.monotonic() - started
            for line in log_input.readlines():
                try:
                    item = json.loads(line)
                except ValueError:
                    continue
                if item.get("msg") == "poll failed":
                    errors.append({"group": item.get("group"), "error": item.get("error")})
                    reason = "polling error"
                if item.get("msg") == "inverter read completed":
                    requests += 1
                    if item.get("success") and elapsed >= 15:
                        latencies.append(item["latency_ms"])
            if reason:
                break
            if process.poll() is not None:
                reason = "proxy exited early"
                break
            if cache.exists():
                try:
                    if db is None:
                        db = sqlite3.connect("file:" + str(cache) + "?mode=ro", uri=True, timeout=0.1)
                    rows = dict(db.execute("SELECT address,updated_at FROM registers WHERE unit_id=1 AND address IN (32064,32106,32000,32016,30000,30073)"))
                    now = dt.datetime.now(dt.timezone.utc).timestamp()
                    for name, address in (FIELDS | SLOW).items():
                        if address not in rows:
                            continue
                        timestamp = dt.datetime.fromisoformat(rows[address]).timestamp()
                        if observed.get(name) != timestamp:
                            observed[name] = timestamp
                            updates[name].append(timestamp)
                        if elapsed >= 15 and name in FIELDS:
                            max_age[name] = max(max_age[name], now - timestamp)
                    if elapsed >= 30 and any(name not in observed for name in updates):
                        reason = "initial fast/slow groups starved"
                    if elapsed >= 30 and any(age > 20 for age in max_age.values()):
                        reason = "fast measurement age exceeded 20s safety margin"
                except sqlite3.OperationalError:
                    pass
            if reason:
                break
            time.sleep(0.1)
    finally:
        process.send_signal(signal.SIGTERM)
        try:
            exit_code = process.wait(timeout=35)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()
            reason = "proxy did not stop within 35s"
            exit_code = -1
        if db:
            db.close()
        log_input.close()
        log_output.close()
    if exit_code != 0 and not reason:
        reason = "nonzero proxy exit"
    if not latencies and not reason:
        reason = "no request timing samples"
    if latencies and statistics.median(latencies) > latency_limit and not reason:
        reason = "median response latency exceeded comparison limit"
    # A single outlier among <20 samples can become the empirical p95; do not
    # mistake that sample-count effect for sustained overload. Baseline traffic
    # also has occasional multi-second replies. Retain all max/gap statistics.
    if len(latencies) >= 30 and quantile(latencies, 0.95) > 6000 and not reason:
        reason = "p95 response latency exceeded 6s ceiling"
    if seconds >= 330 and any(len(updates[key]) < 2 for key in SLOW) and not reason:
        reason = "scheduled slow refresh starved during soak"
    fields = {}
    for name in updates:
        stamps = [stamp for stamp in updates[name] if stamp >= warmup_until]
        gaps = [right - left for left, right in zip(stamps, stamps[1:])]
        fields[name] = {"updates": len(updates[name]), "median_interval_s": round(statistics.median(gaps), 3) if gaps else None,
                        "p95_interval_s": round(quantile(gaps, .95), 3) if gaps else None,
                        "max_interval_s": round(max(gaps), 3) if gaps else None}
    result = {"label": label, "profile": profile, "configured_interval_s": interval,
              "elapsed_s": round(time.monotonic() - started, 1), "passed": reason is None, "reason": reason,
              "requests": requests, "errors": errors,
              "response_ms": {"median": round(statistics.median(latencies), 2) if latencies else None,
                              "p95": quantile(latencies, .95), "max": max(latencies) if latencies else None},
              "max_fast_age_s": {key: round(value, 3) for key, value in max_age.items()}, "fields": fields}
    (directory / "result.json").write_text(json.dumps(result, indent=2))
    print(json.dumps(result), flush=True)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", type=pathlib.Path)
    parser.add_argument("base_yaml", type=pathlib.Path)
    parser.add_argument("output", type=pathlib.Path)
    parser.add_argument("--seconds", type=int, default=90)
    parser.add_argument("--soak-seconds", type=int, default=360)
    parser.add_argument("--profile", choices=["all", "split", "contiguous"], default="all")
    parser.add_argument("--intervals", type=int, nargs="+")
    args = parser.parse_args()
    if args.seconds<45 or args.soak_seconds<330:
        parser.error("Use >=45s comparison stages and >=330s soak to include a scheduled slow refresh")
    if args.intervals and (args.profile=="all" or any(value<1 for value in args.intervals)):
        parser.error("Custom positive intervals require one selected profile")
    args.binary = args.binary.resolve()
    out = args.output.resolve()
    out.mkdir(mode=0o700)
    base = args.base_yaml.read_text()
    for required in ("read_only: true", "read_pause_ms: 1000", "wait_after_connect_ms: 1500", "timeout_ms: 15000"):
        if required not in base:
            raise ValueError("Conservative read-only base configuration required: " + required)
    for port in (1502, 8080, 1883):
        with socket.socket() as check:
            check.settimeout(1)
            if check.connect_ex(("127.0.0.1", port)) == 0:
                raise RuntimeError("Test port already in use: %s" % port)
    results = []
    latency_limit = 2000
    profiles = [("split", [5, 4, 3, 2, 1]), ("contiguous", [3, 2, 1])]
    if args.profile!="all":
        profiles=[(name,args.intervals or intervals) for name,intervals in profiles if name==args.profile]
    for profile, intervals in profiles:
        for interval in intervals:
            label = profile + "-" + str(interval) + "s"
            print(json.dumps({"starting": label, "duration_s": args.seconds}), flush=True)
            result = run_stage(args, base, out, profile, interval, args.seconds, latency_limit, label)
            results.append(result)
            (out / "results.json").write_text(json.dumps(results, indent=2))
            if profile == "split" and interval == 5:
                if not result["passed"]:
                    print("Baseline failed; stopping all load tests", flush=True)
                    return 2
                latency_limit = max(2000, 2 * result["response_ms"]["median"])
            time.sleep(10)
            if not result["passed"]:
                break
    passing = [r for r in results if r["passed"] and r["fields"]["power"]["median_interval_s"] is not None]
    if not passing:
        return 2
    # Prefer a slower configured rate when measured refresh speed is nearly
    # identical; avoid extra load for an imperceptible throughput improvement.
    passing.sort(key=lambda r: r["fields"]["power"]["median_interval_s"])
    best_speed = passing[0]["fields"]["power"]["median_interval_s"]
    candidates = [r for r in passing if r["fields"]["power"]["median_interval_s"] <= best_speed * 1.1]
    winner = max(candidates, key=lambda r: r["configured_interval_s"])
    label = "soak-" + winner["profile"] + "-" + str(winner["configured_interval_s"]) + "s"
    print(json.dumps({"starting": label, "duration_s": args.soak_seconds}), flush=True)
    result = run_stage(args, base, out, winner["profile"], winner["configured_interval_s"], args.soak_seconds, latency_limit, label)
    results.append(result)
    (out / "results.json").write_text(json.dumps(results, indent=2))
    return 0 if result["passed"] else 2


if __name__ == "__main__":
    raise SystemExit(main())
