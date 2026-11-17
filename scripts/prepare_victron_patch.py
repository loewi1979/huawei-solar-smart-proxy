"""Prepare a reviewable aggregate-only patch; never edits the installed driver.

Usage: python scripts/prepare_victron_patch.py upstream.py output.patch
Reviewed against mr-manuel/venus-os_dbus-mqtt-pv
033663ed622c2d7959cde27a03a014463ca1227c (MIT).
"""
import difflib
import pathlib
import sys

original = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8")
anchor = '            logging.debug("PV: {:.1f} W - {:.1f} V - {:.1f} A".format(pv_power, pv_voltage, pv_current))'
assert original.count(anchor) == 1, "Upstream changed; review before generating a patch"
addition = '''            # Huawei has no individual phase active power / energy registers.
            # Opt-in aggregate mode leaves unmeasured D-Bus paths unavailable.
            if config["PV"].get("aggregate_only", "0") == "1":
                self._dbusservice["/Ac/Current"] = None
                self._dbusservice["/Ac/Voltage"] = None
                for phase in ("L1", "L2", "L3"):
                    for field in ("Power", "Current", "Voltage", "Frequency", "PowerFactor", "Energy/Forward"):
                        self._dbusservice["/Ac/" + phase + "/" + field] = None

'''
updated = original.replace(anchor, addition + anchor)
patch = ''.join(difflib.unified_diff(original.splitlines(True), updated.splitlines(True),
                                  fromfile="a/dbus-mqtt-pv/dbus-mqtt-pv.py",
                                  tofile="b/dbus-mqtt-pv/dbus-mqtt-pv.py"))
pathlib.Path(sys.argv[2]).write_text(patch, encoding="utf-8")
pathlib.Path(sys.argv[2] + ".test-driver.py").write_text(updated, encoding="utf-8")
print("Prepared patch and isolated test copy; installed driver unchanged")
