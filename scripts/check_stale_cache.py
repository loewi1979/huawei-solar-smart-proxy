"""Run an offline-only proxy check against an existing persisted test cache.

Usage: python3 check_stale_cache.py BINARY OFFLINE_YAML MQTT_JSON
The offline YAML must use inverter host 127.0.0.1 and port 15020, read_only:
true, fast_max_age_s: 30 and local test listeners 1502, 8080, 1883.
"""
import json
import pathlib
import signal
import subprocess
import sys
import time
import urllib.error
import urllib.request


def main():
    binary, config, mqtt = sys.argv[1:]
    text = pathlib.Path(config).read_text()
    # This harness is intentionally restricted to the known offline test setup.
    for required in ('host: "127.0.0.1"', 'port: 15020', 'read_only: true', 'fast_max_age_s: 30'):
        if required not in text:
            raise ValueError("Not the expected loopback/read-only outage configuration")
    log = pathlib.Path("stale-check.log").open("w")
    proxy = subprocess.Popen([str(pathlib.Path(binary).resolve()), "-config", config], stdout=log, stderr=log)
    base = "http://127.0.0.1:8080"
    try:
        for _ in range(30):
            if proxy.poll() is not None:
                raise RuntimeError("Offline test proxy exited early")
            try:
                with urllib.request.urlopen(base + "/healthz", timeout=1) as response:
                    if response.status == 200:
                        break
            except urllib.error.URLError:
                time.sleep(0.2)
        else:
            raise RuntimeError("Offline test proxy did not start")
        time.sleep(32)
        probe = subprocess.run([sys.executable, "read_only_probe.py", "127.0.0.1", "--port", "1502", "--unit", "1"], capture_output=True, text=True, timeout=10)
        report = json.loads(probe.stdout)
        fast = [x for x in report["reads"] if x["name"] in ("grid_power", "total_energy")]
        if probe.returncode != 2 or len(fast) != 2 or any(x.get("error") != "Modbus exception 0x0b" for x in fast):
            raise AssertionError("Persisted stale data were served: " + probe.stdout)
        try:
            urllib.request.urlopen(base + "/readyz", timeout=2)
            raise AssertionError("Stale proxy incorrectly ready")
        except urllib.error.HTTPError as error:
            if error.code != 503:
                raise
        try:
            result = subprocess.run([sys.executable, "check_mqtt_sample.py", mqtt], capture_output=True, text=True, timeout=7)
            raise AssertionError("Expected MQTT silence, got exit %s: %s" % (result.returncode, result.stdout + result.stderr))
        except subprocess.TimeoutExpired:
            pass
        with urllib.request.urlopen(base + "/healthz", timeout=2) as response:
            if response.status != 200:
                raise AssertionError("Proxy stopped while checking MQTT silence")
        print(json.dumps({"persisted_stale_fast_reads": "rejected with 0x0b", "readiness_http": 503, "mqtt_data": "silent for 7s with broker alive", "hardware_access": False}, indent=2))
    finally:
        proxy.send_signal(signal.SIGTERM)
        try:
            code = proxy.wait(timeout=20)
        except subprocess.TimeoutExpired:
            proxy.kill()
            proxy.wait()
            raise
        log.close()
        if code != 0:
            raise RuntimeError("Proxy did not shut down cleanly: %s" % code)


if __name__ == "__main__":
    main()
