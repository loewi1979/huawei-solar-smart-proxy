"""Small read-only SUN2000 Modbus TCP probe (Python standard library only).

Only function 0x03 exists in this script. No write function is implemented.
Run only when access to the target dongle is coordinated with existing readers.
"""
import argparse
import datetime
import json
import socket
import struct
import time


def recv_exact(conn, count):
    data = b""
    while len(data) < count:
        block = conn.recv(count - len(data))
        if not block:
            raise ConnectionError("Modbus peer closed the connection")
        data += block
    return data


def read_holding(conn, tx, unit, address, count):
    request = struct.pack(">HHHBBHH", tx, 0, 6, unit, 3, address, count)
    conn.sendall(request)
    transaction, protocol, length, slave = struct.unpack(">HHHB", recv_exact(conn, 7))
    if transaction != tx or protocol != 0 or slave != unit or not 2 <= length <= 254:
        raise ValueError("Invalid Modbus header")
    pdu = recv_exact(conn, length - 1)
    if pdu[0] == 0x83:
        raise ValueError("Modbus exception 0x%02x" % pdu[1])
    if pdu[0] != 3 or pdu[1] != count * 2 or len(pdu) != count * 2 + 2:
        raise ValueError("Invalid Modbus read response")
    return pdu[2:]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("host")
    parser.add_argument("--port", type=int, default=502)
    parser.add_argument("--unit", type=int, default=1)
    args = parser.parse_args()
    if not 1 <= args.unit <= 247 or not 1 <= args.port <= 65535:
        parser.error("Invalid port or unit ID")
    result = {"time_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(), "unit": args.unit, "read_only": True, "reads": []}
    failed = False
    with socket.create_connection((args.host, args.port), timeout=5) as conn:
        conn.settimeout(5)
        time.sleep(1)
        for tx, (name, address, count) in enumerate(
            [("model", 30000, 15), ("grid_power", 32064, 33), ("total_energy", 32106, 2)], 1
        ):
            started = time.monotonic()
            item = {"name": name, "address": address, "count": count}
            try:
                raw = read_holding(conn, tx, args.unit, address, count)
                item["latency_ms"] = round((time.monotonic() - started) * 1000, 1)
                if name == "model":
                    item["model"] = raw.split(b"\0", 1)[0].decode("ascii", errors="replace").strip()
                elif name == "grid_power":
                    item["active_power_w"] = struct.unpack(">i", raw[32:36])[0]
                    item["device_status_raw"] = struct.unpack(">H", raw[50:52])[0]
                else:
                    item["total_yield_kwh"] = struct.unpack(">I", raw)[0] / 100
            except (ValueError, OSError) as error:
                failed = True
                item["error"] = str(error)
            result["reads"].append(item)
            time.sleep(1)
    print(json.dumps(result, indent=2))
    return 2 if failed else 0


if __name__ == "__main__":
    raise SystemExit(main())
