"""Bounded health log for the standalone read-only soak service.

No Modbus request is sent by this observer. MQTT uses its credentials file;
measurement values are discarded. web_url defaults to loopback and can target
the configured LAN listener. Run --once for an install check.
"""
import argparse
import base64
import datetime
import json
import logging
from logging.handlers import RotatingFileHandler
import pathlib
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request


def check_http(path, token, base_url="http://127.0.0.1:8080"):
    request = urllib.request.Request(base_url.rstrip("/") + path)
    auth = base64.b64encode(("admin:" + token).encode()).decode()
    request.add_header("Authorization", "Basic " + auth)
    try:
        with urllib.request.urlopen(request, timeout=5) as response:
            return response.status
    except urllib.error.HTTPError as error:
        return error.code
    except (OSError, urllib.error.URLError):
        return 0


def check_read_only(token, base_url="http://127.0.0.1:8080"):
    request = urllib.request.Request(base_url.rstrip("/") + "/")
    auth = base64.b64encode(("admin:" + token).encode()).decode()
    request.add_header("Authorization", "Basic " + auth)
    try:
        with urllib.request.urlopen(request, timeout=5) as response:
            match = re.search(r"Read Only</dt>\s*<dd>(true|false)</dd>", response.read().decode())
            return match[1] == "true" if match else None
    except (OSError, urllib.error.URLError):
        return None


def sample(config):
    started = time.monotonic()
    base_url = config.get("web_url", "http://127.0.0.1:8080")
    result = {"time_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
              "health_http": check_http("/healthz", config["web_token"], base_url),
              "ready_http": check_http("/readyz", config["web_token"], base_url)}
    result["read_only"] = check_read_only(config["web_token"], base_url)
    try:
        state = subprocess.run(["/usr/bin/systemctl", "show", "huawei-smart-proxy.service",
                                "-p", "ActiveState", "-p", "SubState", "-p", "NRestarts"],
                               capture_output=True, text=True, timeout=5)
        result["service"] = dict(line.split("=", 1) for line in state.stdout.splitlines() if "=" in line)
    except (OSError, subprocess.TimeoutExpired):
        result["service"] = {"ActiveState": "unknown"}
    try:
        message = subprocess.run([sys.executable, config["mqtt_probe"], config["mqtt_credentials"]],
                                 capture_output=True, text=True, timeout=12)
        payload = json.loads(message.stdout) if message.returncode == 0 else {}
        result["mqtt_sample_ok"] = message.returncode == 0 and payload.get("retained") is False and payload.get("qos") == 0
    except (OSError, ValueError, subprocess.TimeoutExpired):
        result["mqtt_sample_ok"] = False
    result["check_duration_s"] = round(time.monotonic() - started, 3)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("config", type=pathlib.Path)
    parser.add_argument("--once", action="store_true")
    args = parser.parse_args()
    config = json.loads(args.config.read_text())
    output = pathlib.Path(config["output_directory"])
    output.mkdir(mode=0o700, parents=True, exist_ok=True)
    logger = logging.getLogger("test-health")
    logger.setLevel(logging.INFO)
    handler = RotatingFileHandler(output / "health.jsonl", maxBytes=4 * 1024 * 1024, backupCount=3)
    handler.setFormatter(logging.Formatter("%(message)s"))
    logger.addHandler(handler)
    while True:
        started = time.monotonic()
        result = sample(config)
        logger.info(json.dumps(result, separators=(",", ":")))
        temporary = output / "latest.tmp"
        temporary.write_text(json.dumps(result, indent=2))
        temporary.replace(output / "latest.json")
        if args.once:
            print(json.dumps(result, indent=2))
            return
        time.sleep(max(1, 60 - (time.monotonic() - started)))


if __name__ == "__main__":
    main()
