"""Verify one fresh QoS-0 Victron payload on an isolated MQTT test broker.

Usage: python3 scripts/check_mqtt_sample.py private-test-credentials.json
JSON keys: host, port, username, password, topic. Credentials are never printed.
"""
import json
import pathlib
import socket
import struct
import sys
import uuid


def utf(value):
    raw = value.encode()
    return struct.pack(">H", len(raw)) + raw


def packet(kind, body):
    length = len(body)
    remaining = bytearray()
    while True:
        digit = length % 128
        length //= 128
        remaining.append(digit | (128 if length else 0))
        if not length:
            break
    return bytes([kind]) + remaining + body


def read_exact(conn, count):
    data = b""
    while len(data) < count:
        chunk = conn.recv(count - len(data))
        if not chunk:
            raise ConnectionError("MQTT peer closed connection")
        data += chunk
    return data


def read_packet(conn):
    kind = read_exact(conn, 1)[0]
    length = 0
    multiplier = 1
    for _ in range(4):
        digit = read_exact(conn, 1)[0]
        length += (digit & 127) * multiplier
        if not digit & 128:
            if length > 65536:
                raise ValueError("MQTT packet exceeds test limit")
            return kind, read_exact(conn, length)
        multiplier *= 128
    raise ValueError("Malformed MQTT length")


def main():
    config = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8-sig"))
    with socket.create_connection((config["host"], config["port"]), timeout=20) as conn:
        conn.settimeout(20)
        body = utf("MQTT") + bytes([4, 0xC2, 0, 30])
        body += utf("proxy-check-" + uuid.uuid4().hex[:12]) + utf(config["username"]) + utf(config["password"])
        conn.sendall(packet(0x10, body))
        if read_packet(conn) != (0x20, b"\x00\x00"):
            raise ValueError("MQTT authentication failed")
        conn.sendall(packet(0x82, b"\x00\x01" + utf(config["topic"]) + b"\x00"))
        while True:
            kind, body = read_packet(conn)
            if kind == 0x90:
                if body != b"\x00\x01\x00":
                    raise ValueError("MQTT subscription rejected")
                continue
            if kind >> 4 != 3:
                raise ValueError("Expected PUBLISH")
            if kind & 7:
                raise ValueError("Telemetry is retained or not QoS 0")
            topic_size = struct.unpack(">H", body[:2])[0]
            if body[2:2 + topic_size].decode() != config["topic"]:
                raise ValueError("Unexpected topic")
            payload = json.loads(body[2 + topic_size:])
            if set(payload) != {"pv"} or set(payload["pv"]) != {"power", "energy_forward"}:
                raise ValueError("Unexpected or invented fields in aggregate payload")
            if not all(isinstance(x, (int, float)) for x in payload["pv"].values()):
                raise ValueError("Non-numeric measurement")
            print(json.dumps({"topic": config["topic"], "retained": False, "qos": 0, "payload": payload}, indent=2))
            conn.sendall(b"\xe0\x00")
            return


if __name__ == "__main__":
    main()
