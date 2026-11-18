"""Prepare a reviewable aggregate-only patch; never edits the installed driver.

Usage: python scripts/prepare_victron_patch.py upstream.py output.patch
Reviewed against mr-manuel/venus-os_dbus-mqtt-pv
033663ed622c2d7959cde27a03a014463ca1227c (MIT).
"""
import difflib
import pathlib
import sys

original = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8")
global_anchor = '        global pv_L3_power, pv_L3_current, pv_L3_voltage, pv_L3_frequency, pv_L3_power_factor, pv_L3_forward'
capture_anchor = '                last_changed = int(time())'
assert original.count(global_anchor) == 1 and original.count(capture_anchor) == 1, "Upstream changed; review capture anchors"
capture = '''                # Preserve measured phase diagnostics independently of phase power.
                if config["PV"].get("aggregate_only", "0") == "1" and isinstance(jsonpayload.get("pv"), dict) and "power" in jsonpayload["pv"]:
                    pv_measured_phases = {}
                    for phase in ("L1", "L2", "L3"):
                        raw = jsonpayload["pv"].get(phase, {})
                        pv_measured_phases[phase] = {}
                        if isinstance(raw, dict):
                            for field in ("voltage", "current", "frequency", "power"):
                                if field == "power" and raw.get("power_is_estimate") is not True:
                                    continue
                                value = raw.get(field)
                                if type(value) in (int, float) and value == value and abs(value) != float("inf"):
                                    pv_measured_phases[phase][field] = float(value)

'''
anchor = '            logging.debug("PV: {:.1f} W - {:.1f} V - {:.1f} A".format(pv_power, pv_voltage, pv_current))'
assert original.count(anchor) == 1, "Upstream changed; review before generating a patch"
addition = '''            # Huawei has no individual phase active power / energy registers.
            # Keep measured phase diagnostics; clear all unmeasured paths.
            if config["PV"].get("aggregate_only", "0") == "1":
                self._dbusservice["/Ac/Current"] = None
                self._dbusservice["/Ac/Voltage"] = None
                for phase in ("L1", "L2", "L3"):
                    for field in ("Power", "Current", "Voltage", "Frequency", "PowerFactor", "Energy/Forward"):
                        self._dbusservice["/Ac/" + phase + "/" + field] = None
                    measured = globals().get("pv_measured_phases", {}).get(phase, {})
                    for wire, path in (("voltage", "Voltage"), ("current", "Current"), ("frequency", "Frequency"), ("power", "Power")):
                        self._dbusservice["/Ac/" + phase + "/" + path] = measured.get(wire)

'''
updated = original.replace(global_anchor, global_anchor + '\n        global pv_measured_phases')
updated = updated.replace(capture_anchor, capture + capture_anchor)
updated = updated.replace(anchor, addition + anchor)
patch = ''.join(difflib.unified_diff(original.splitlines(True), updated.splitlines(True),
                                  fromfile="a/dbus-mqtt-pv/dbus-mqtt-pv.py",
                                  tofile="b/dbus-mqtt-pv/dbus-mqtt-pv.py"))
pathlib.Path(sys.argv[2]).write_text(patch, encoding="utf-8")
pathlib.Path(sys.argv[2] + ".test-driver.py").write_text(updated, encoding="utf-8")
print("Prepared patch and isolated test copy; installed driver unchanged")
