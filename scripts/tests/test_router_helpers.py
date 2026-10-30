import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
SH = "C:/Program Files/Git/bin/bash.exe" if os.name == "nt" else shutil.which("sh")


class RouterHelpers(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)
        self.env = os.environ.copy()
        self.history = self.dir / "history"
        self.boot_file = self.dir / "boot-id"
        self.env.update(UAMASK_HISTORY_DIR=self.history.as_posix(),
                        UAMASK_BOOT_ID_FILE=self.boot_file.as_posix())

    def run_helper(self, name, *args, success=True):
        result = subprocess.run([SH, (ROOT / "files" / name).as_posix(), *args],
                                env=self.env, capture_output=True, text=True, timeout=20)
        if success:
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0)
        return result

    def boot(self, number):
        value = f"00000000-0000-0000-0000-{number:012x}"
        self.boot_file.write_text(value + "\n", newline="\n")
        return value

    def test_ttl_validates_values_and_optional_ipv6(self):
        text = self.run_helper("uamask-ttl.sh", "render", "eth1", "64", "0").stdout
        self.assertIn('oifname "eth1"', text)
        self.assertIn("ip ttl set 64", text)
        self.assertNotIn("hoplimit", text)
        self.assertNotIn("tcp", text)  # TTL applies to UDP and bypassed traffic too.
        text = self.run_helper("uamask-ttl.sh", "render", "pppoe-wan", "65", "1").stdout
        self.assertIn("ip6 hoplimit set 65", text)
        for device, ttl in [("eth1;bad", "64"), ("eth1", "0"), ("eth1", "256"), ("eth1", "64;bad")]:
            self.run_helper("uamask-ttl.sh", "render", device, ttl, "0", success=False)

    def test_boot_boundaries_and_same_boot_restart(self):
        first = self.boot(1)
        self.run_helper("uamask-log-history.sh", "prepare")
        old_log = self.history / "boots" / first / "system.log"
        old_log.write_text("previous boot evidence\n")
        self.run_helper("uamask-log-history.sh", "prepare")
        self.assertEqual(self.history.joinpath("boots.index").read_text().splitlines(), [first])
        second = self.boot(2)
        self.run_helper("uamask-log-history.sh", "prepare")
        self.assertEqual(old_log.read_text(), "previous boot evidence\n")
        self.assertEqual(self.history.joinpath("current-boot").read_text().strip(), second)

    def test_quota_removes_oldest_boot_and_reserves_active_rotation(self):
        ids = []
        for number in range(1, 8):
            boot = self.boot(number)
            ids.append(boot)
            self.run_helper("uamask-log-history.sh", "prepare")
            directory = self.history / "boots" / boot
            for filename in ("system.log", "system.log.old"):
                (directory / filename).write_bytes(b"s" * (512 * 1024 + 4096))
            for filename in ("resources.log", "resources.log.old"):
                (directory / filename).write_bytes(b"r" * (64 * 1024 + 4096))
            self.run_helper("uamask-log-history.sh", "prune")
            total = sum(p.stat().st_size for p in self.history.rglob("*") if p.is_file())
            self.assertLessEqual(total, 5 * 1024 * 1024)
        self.assertFalse((self.history / "boots" / ids[0]).exists())
        self.assertTrue((self.history / "boots" / ids[-1]).exists())

    def test_boot_id_cannot_escape_history_root(self):
        self.boot_file.write_text("../../outside\n")
        self.run_helper("uamask-log-history.sh", "prepare", success=False)
        self.assertFalse(self.history.exists())

    def test_offload_state_is_saved_once_and_restored(self):
        bindir = self.dir / "bin"
        bindir.mkdir()
        cfg = self.dir / "cfg"
        cfg.mkdir()
        self.env.update(PATH=bindir.as_posix() + os.pathsep + self.env["PATH"],
                        UAMASK_TEST_UCI=cfg.as_posix(),
                        UAMASK_TTL_STATE=(self.dir / "offload.state").as_posix(),
                        UAMASK_TTL_RULE=(self.dir / "ttl.nft").as_posix())
        (bindir / "uci").write_text('''#!/bin/sh
set -eu
[ "$1" != -q ] || shift
op=$1; shift
case "$op" in
 get) cat "$UAMASK_TEST_UCI/$1";;
 set) printf '%s\\n' "${1#*=}" >"$UAMASK_TEST_UCI/${1%%=*}";;
 delete) rm -f "$UAMASK_TEST_UCI/$1";;
 commit) :;;
 *) exit 2;;
esac
''', newline="\n")
        (bindir / "fw4").write_text("#!/bin/sh\nexit 0\n", newline="\n")
        for p in bindir.iterdir():
            p.chmod(0o755)
        values = {"UAmask.main.ttl_enabled": "1", "UAmask.main.ttl_device": "eth1",
                  "UAmask.main.ttl_value": "64", "UAmask.main.ttl_ipv6": "0",
                  "firewall.@defaults[0].flow_offloading": "1"}
        for key, value in values.items():
            (cfg / key).write_text(value + "\n", newline="\n")
        self.run_helper("uamask-ttl.sh", "prepare")
        state = Path(self.env["UAMASK_TTL_STATE"])
        self.assertEqual(state.read_text().splitlines(), ["1", "unset"])
        self.run_helper("uamask-ttl.sh", "prepare")
        self.assertEqual(state.read_text().splitlines(), ["1", "unset"])
        self.assertEqual((cfg / "firewall.@defaults[0].flow_offloading").read_text().strip(), "0")
        self.run_helper("uamask-ttl.sh", "restore")
        self.assertFalse(state.exists())
        self.assertEqual((cfg / "firewall.@defaults[0].flow_offloading").read_text().strip(), "1")
        self.assertFalse((cfg / "firewall.@defaults[0].flow_offloading_hw").exists())


if __name__ == "__main__":
    unittest.main()
