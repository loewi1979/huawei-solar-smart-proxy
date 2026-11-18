"""Run extracted upstream receiver logic without MQTT, D-Bus or a GX device.

Usage: python scripts/check_victron_driver.py /path/to/dbus-mqtt-pv.py
This validates receiver semantics; it is not a Venus OS integration test.
"""
import ast
import json
import logging
import sys
from types import SimpleNamespace

source = open(sys.argv[1], encoding="utf-8").read()
tree = ast.parse(source)
receiver = next(n for n in tree.body if isinstance(n, ast.FunctionDef) and n.name == "on_message")
service = next(n for n in tree.body if isinstance(n, ast.ClassDef) and n.name == "DbusMqttPvService")
update = next(n for n in service.body if isinstance(n, ast.FunctionDef) and n.name == "_update")
scope = dict(
    time=lambda: 161, logging=logging, sys=sys, json=json,
    config={"MQTT": {"topic": "huawei_wechselrichter"}, "DEFAULT": {"voltage": "230", "frequency": "50"}, "PV": {"aggregate_only": "1" if "--aggregate-only" in sys.argv else "0"}},
    last_changed=100, last_updated=100, timeout=60, standby_power=0,
)
for phase in ("", "L1_", "L2_", "L3_"):
    for field in ("power", "current", "voltage", "forward", "frequency", "power_factor"):
        scope["pv_" + phase + field] = None
exec(compile(ast.Module(body=[receiver, update], type_ignores=[]), "upstream-receiver", "exec"), scope)
fake = SimpleNamespace(_dbusservice={"/Ac/Power": 1234, "/UpdateIndex": 0, "/StatusCode": 7})
try:
    scope["_update"](fake)
    raise AssertionError("Receiver failed to exit after >60 seconds silence")
except SystemExit:
    pass
scope["timeout"] = 0
assert scope["_update"](fake) is True
assert fake._dbusservice["/Ac/Power"] == 1234, "Expected stale power to remain when timeout is disabled"
scope["timeout"] = 60
msg = SimpleNamespace(topic="huawei_wechselrichter", payload=b'{"pv":{"power":1234,"energy_forward":123.45}}')
scope["on_message"](None, None, msg)
scope["_update"](fake)
assert fake._dbusservice["/Ac/Power"] == 1234
if "--aggregate-only" in sys.argv:
    assert fake._dbusservice["/Ac/L1/Power"] is None
    assert fake._dbusservice["/Ac/L1/Energy/Forward"] is None
    assert fake._dbusservice["/Ac/Current"] is None
    assert fake._dbusservice["/Ac/Voltage"] is None
else:
    assert fake._dbusservice["/Ac/L1/Power"] == 1234, "Upstream aggregate-to-L1 fallback changed"
    assert fake._dbusservice["/Ac/L1/Energy/Forward"] == 123.45
assert scope["pv_L1_power_factor"] is None
assert scope["pv_L2_forward"] is None and scope["pv_L3_forward"] is None
phases = {"L1": {"voltage": 230.7, "current": 12.345, "frequency": 50.01},
          "L2": {"voltage": 231.2, "current": 12.1, "frequency": 50.01},
          "L3": {"voltage": 229.8, "current": 0.0, "frequency": 50.01}}
wire = {"pv": {"power": 1234, "energy_forward": 123.45, **phases}}
scope["time"] = lambda: 162
scope["on_message"](None, None, SimpleNamespace(topic="huawei_wechselrichter", payload=json.dumps(wire).encode()))
scope["_update"](fake)
if "--aggregate-only" in sys.argv:
    for phase, values in phases.items():
        for field, value in values.items():
            assert fake._dbusservice["/Ac/" + phase + "/" + field.capitalize()] == value
        for field in ("Power", "PowerFactor", "Energy/Forward"):
            assert fake._dbusservice["/Ac/" + phase + "/" + field] is None
    # Omitted optional fields must clear old measurements, not retain them.
    wire = {"pv": {"power": 1234, "energy_forward": 123.45, "L1": {"voltage": 230.1}}}
    scope["time"] = lambda: 163
    scope["on_message"](None, None, SimpleNamespace(topic="huawei_wechselrichter", payload=json.dumps(wire).encode()))
    scope["_update"](fake)
    assert fake._dbusservice["/Ac/L1/Voltage"] == 230.1
    assert fake._dbusservice["/Ac/L1/Current"] is None
    assert fake._dbusservice["/Ac/L2/Voltage"] is None
else:
    assert fake._dbusservice["/Ac/L1/Voltage"] == 230, "Stock driver unexpectedly read phase voltage without phase power"
scope["time"] = lambda: 164
wire = {"pv": {"power": 1234, "energy_forward": 123.45,
               "L1": {"power": 1000, "power_is_estimate": True, "voltage": 230.7, "current": 4.4, "frequency": 50.01},
               "L2": {"power": 2000, "voltage": 231.2, "current": 8.7}}}
scope["on_message"](None, None, SimpleNamespace(topic="huawei_wechselrichter", payload=json.dumps(wire).encode()))
scope["_update"](fake)
assert fake._dbusservice["/Ac/Power"] == 1234
assert fake._dbusservice["/Ac/L1/Power"] == 1000
assert fake._dbusservice["/Ac/L1/Voltage"] == 230.7
if "--aggregate-only" in sys.argv:
    assert fake._dbusservice["/Ac/L2/Power"] is None, "Unlabelled estimate accepted in aggregate mode"
    assert fake._dbusservice["/Ac/L1/Energy/Forward"] is None
    scope["time"] = lambda: 165
    scope["on_message"](None, None, SimpleNamespace(topic="huawei_wechselrichter", payload=b'{"pv":{"power":1234,"energy_forward":123.45}}'))
    scope["_update"](fake)
    assert fake._dbusservice["/Ac/L1/Power"] is None, "Omitted estimate held stale"
last_sample_time = scope["last_changed"]
scope["time"] = lambda: 180
scope["on_message"](None, None, SimpleNamespace(topic="huawei_wechselrichter/status", payload=b'"stale"'))
assert scope["last_changed"] == last_sample_time, "Status topic must not reset measurement timeout"
print("PASS: timeout exit; stale hold; aggregate policy; measured phase diagnostics; omitted fields cleared; no invented phase PF; status timeout")
