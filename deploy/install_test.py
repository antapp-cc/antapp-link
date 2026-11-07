"""只在临时目录验证安装片段，不运行真实安装。Linux/WSL 运行：python3 deploy/install_test.py；依赖 bash 和 openssl（Ed25519）。"""
import base64
import hashlib
import json
import pathlib
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

SOURCE = pathlib.Path(__file__).with_name("install.sh").read_text(encoding="utf-8")
VERIFY = SOURCE.split("<<'PY'\n", 1)[1].split("\nPY\n", 1)[0]
FETCH = SOURCE.split("fetch_release_file() {", 1)[1].split("\n}\n", 1)[0]


def require_test_environment():
    if sys.platform != "linux":
        raise RuntimeError("安装隔离回归仅支持 Linux/WSL；Windows 请运行：wsl --exec python3 deploy/install_test.py")
    missing = [name for name in ("bash", "openssl", "flock") if shutil.which(name) is None]
    if missing:
        raise RuntimeError("安装隔离回归缺少依赖：" + ", ".join(missing) + "；请在 Linux/WSL 安装后重试")


class InstallReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.folder = pathlib.Path(self.temp.name)
        self.key = self.folder / "test.key"
        subprocess.run(["openssl", "genpkey", "-algorithm", "Ed25519", "-out", str(self.key)], check=True, capture_output=True)
        pub = subprocess.check_output(["openssl", "pkey", "-in", str(self.key), "-pubout", "-outform", "DER"])
        self.script = re.sub(r'base64.b64decode\("[A-Za-z0-9+/=]+"\)', 'base64.b64decode("' + base64.b64encode(pub).decode() + '")', VERIFY, count=1)
        self.binary = bytearray(80)
        self.binary[:7] = bytes.fromhex("7f454c46020101")
        self.binary[16:24] = bytes.fromhex("02003e0001000000")
        self.manifest = {"version": "1.2.3", "server": {"url": "https://github.com/antapp-cc/antapp-link/releases/download/v1.2.3/antapp-linkd", "size": len(self.binary), "sha256": hashlib.sha256(self.binary).hexdigest()}}
        self.sign()

    def sign(self):
        (self.folder / "antapp-linkd").write_bytes(self.binary)
        manifest = self.folder / "latest.json"
        manifest.write_text(json.dumps(self.manifest), encoding="utf-8", newline="\n")
        sig = self.folder / "sig.raw"
        subprocess.run(["openssl", "pkeyutl", "-sign", "-rawin", "-inkey", str(self.key), "-in", str(manifest), "-out", str(sig)], check=True, capture_output=True)
        (self.folder / "latest.json.sig").write_text(base64.b64encode(sig.read_bytes()).decode(), encoding="utf-8", newline="\n")

    def verify(self, expected, tag="", phase="binary"):
        result = subprocess.run([sys.executable, "-c", self.script, str(self.folder), phase, tag], capture_output=True, text=True, encoding="utf-8")
        self.assertEqual(result.returncode == 0, expected, result.stdout + result.stderr)

    def test_valid_signature(self):
        self.verify(True)
        self.verify(True, "v1.2.3", "metadata")

    def test_production_public_key_matches_go(self):
        go = pathlib.Path(__file__).parents[1].joinpath("internal/update/pubkey.go").read_text(encoding="utf-8")
        key = bytes(int(s, 16) for s in re.findall(r"0x([0-9a-f]{2})", go))
        der = base64.b64decode(re.search(r'base64.b64decode\("([A-Za-z0-9+/=]+)"\)', VERIFY)[1])
        self.assertEqual(der[-32:], key)

    def test_partial_binary(self):
        (self.folder / "antapp-linkd").write_bytes(bytes.fromhex("7f454c46"))
        self.verify(False)

    def test_tampered_manifest(self):
        (self.folder / "latest.json").write_text('{}', encoding='utf-8', newline='\n')
        self.verify(False)

    def test_missing_or_wrong_signature(self):
        (self.folder / "latest.json.sig").write_text('', encoding='utf-8', newline='\n')
        self.verify(False)
        (self.folder / "latest.json.sig").unlink()
        self.verify(False)

    def test_wrong_platform_even_with_valid_signature(self):
        self.binary[18:20] = bytes.fromhex("b700")
        self.manifest["server"]["sha256"] = hashlib.sha256(self.binary).hexdigest()
        self.sign()
        self.verify(False)

    def test_wrong_version_or_url(self):
        self.verify(False, "v9.9.9")
        self.manifest["server"]["url"] = "https://example.invalid/untrusted"
        self.sign()
        self.verify(False)

    def test_failed_download_removes_partial(self):
        target = self.folder / "partial"
        script = "fetch_release_file() {" + FETCH + "\n}\n" + r'''
timeout() { shift; "$@"; }
wget() { while [[ "$1" != "-qO" ]]; do shift; done; shift; printf '\177ELF' > "$1"; return 8; }
fetch_release_file "$1" https://example.invalid/file
'''
        result = subprocess.run(["bash", "-c", script, "test", str(target)])
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(target.exists())

    def test_service_start_must_be_stable(self):
        block = SOURCE.split('log "启动隧道"\n', 1)[1].split('\nlog "当前状态"', 1)[0]
        helper = ""
        if "wait_install_service() {" in SOURCE:
            helper = "wait_install_service() {" + SOURCE.split("wait_install_service() {", 1)[1].split("\n}\n", 1)[0] + "\n}\n"
        for scenario, expected in (("inactive", False), ("crashing", False), ("changing_pid", False), ("unreadable", False), ("restarted", True), ("stable", True)):
            with self.subTest(scenario=scenario):
                script = r'''set -e
log(){ :; }
die(){ exit 1; }
sleep(){ :; }
SCENARIO="$1"
COUNTER="$2"
systemctl(){
  case "$1" in
    restart) return 0 ;;
    is-active) [[ "$SCENARIO" != inactive ]] ;;
    show)
      [[ "$SCENARIO" != unreadable ]] || return 1
      case "$3" in
        MainPID)
          if [[ "$SCENARIO" == changing_pid ]]; then
            local n=1
            [[ ! -f "$COUNTER" ]] || n=$(cat "$COUNTER")
            echo "$((n+1))" > "$COUNTER"; echo "$((100+n))"
          else echo 101; fi ;;
        NRestarts)
          if [[ "$SCENARIO" == crashing ]]; then
            local n=0
            [[ ! -f "$COUNTER.nr" ]] || n=$(cat "$COUNTER.nr")
            n=$((n+1)); echo "$n" > "$COUNTER.nr"; echo "$n"
          elif [[ "$SCENARIO" == restarted ]]; then echo 3
          else echo 0; fi ;;
      esac ;;
  esac
}
''' + helper + block
                counter = self.folder / "pid-counter"
                result = subprocess.run(["bash", "-c", script, "test", scenario, str(counter)], capture_output=True, text=True, encoding="utf-8")
                self.assertEqual(result.returncode == 0, expected, result.stdout + result.stderr)

    def rollback_case(self, *, had_old=True, active=True, attempted=True, derived=False, existed=True, restore_failure=False, restart_failure=False, health_failure=False, sysctl_failure=False):
        folder = self.folder / ("case-" + str(len(list(self.folder.iterdir()))))
        folder.mkdir()
        binary, stage, calls = folder / "installed", folder / "stage", folder / "calls"
        stage.mkdir()
        old = b'#!/bin/sh\nprintf "old:%s\\n" "$*" >> "$CALLS"\n'
        new = b'#!/bin/sh\nprintf "new:%s\\n" "$*" >> "$CALLS"\n'
        (stage / "antapp-linkd").write_bytes(new)
        if had_old:
            binary.write_bytes(old)
            binary.chmod(0o755)
        config = folder / "server.json"
        if existed:
            config.write_text("old configuration", encoding="utf-8")
            config.chmod(0o640)
        derived_file = folder / "etc/dnsmasq.d/antapp.conf"
        derived_file.parent.mkdir(parents=True)
        derived_file.write_text("old DNS", encoding="utf-8")
        block = "had_old=0\n" + SOURCE.split("had_old=0\n", 1)[1].split('\nlog "初始化配置', 1)[0]
        # 所有生产绝对路径重定向到临时目录；绝不对宿主配置执行恢复。
        block = block.replace("/etc/", str(folder / "etc") + "/").replace("/run/antapp-link/", str(folder / "run") + "/")
        script = r'''set -euo pipefail
BIN="$1"; stage="$2"; CALLS="$3"; ACTIVE="$4"; CONF="$5"; CONF_DIR="$(dirname "$CONF")"
export CALLS
log(){ printf '%s\n' "$*"; }
wait_install_service(){ [[ "${HEALTH_FAIL:-0}" == 0 ]]; }
systemctl(){
  printf 'systemctl:%s\n' "$*" >> "$CALLS"
  case "$1" in
    is-active) [[ "$ACTIVE" == 1 && "$3" == antapp-linkd ]]; return $? ;;
    is-enabled) echo disabled; return 1 ;;
    restart) if [[ "$2" == antapp-linkd ]]; then printf 'restart-config:%s\n' "$(cat "$CONF")" >> "$CALLS"; [[ "${RESTART_FAIL:-0}" == 0 ]] || return 1; fi ;;
  esac
  return 0
}
sysctl(){ if [[ "$1" == -n ]]; then echo 1; else printf 'sysctl:%s\n' "$*" >> "$CALLS"; [[ "${SYSCTL_FAIL:-0}" == 0 ]] || return 1; fi; }
''' + block + '\nprintf new-configuration > "$CONF"\n'
        if derived:
            script += 'install_state_modified=1; netfilter_attempted=1\nprintf new-DNS > "' + str(derived_file) + '"\n'
        if restore_failure:
            script += 'cp(){ if [[ "$1" == -a && "$2" == -- && "$3" == "$stage/rollback/file-0" ]]; then return 1; fi; command cp "$@"; }\n'
        script += "RESTART_FAIL=" + str(int(restart_failure)) + "; HEALTH_FAIL=" + str(int(health_failure)) + "; SYSCTL_FAIL=" + str(int(sysctl_failure)) + "\n"
        script += "service_restart_attempted=" + str(int(attempted)) + "\nexit 7\n"
        result = subprocess.run(["bash", "-c", script, "test", str(binary), str(stage), str(calls), str(int(active)), str(config)], capture_output=True, text=True, encoding="utf-8")
        self.assertEqual(result.returncode, 7, result.stdout + result.stderr)
        self.assertEqual(binary.exists(), had_old)
        if had_old:
            self.assertEqual(binary.read_bytes(), old)
        if restore_failure or restart_failure or health_failure or sysctl_failure:
            self.assertTrue((stage / "rollback/file-0").exists(), result.stdout)
            self.assertIn("恢复资料保留", result.stdout)
        else:
            self.assertEqual(config.exists(), existed)
            if existed:
                self.assertEqual(config.read_text(encoding="utf-8"), "old configuration")
                self.assertEqual(config.stat().st_mode & 0o777, 0o640)
            self.assertEqual(derived_file.read_text(encoding="utf-8"), "old DNS")
            self.assertFalse(stage.exists(), result.stdout)
        return calls.read_text(encoding="utf-8"), result.stdout

    def test_rollback_restores_previous_service_state(self):
        for had_old, active, attempted in ((True, True, True), (True, False, True), (False, False, True), (True, True, False)):
            with self.subTest(had_old=had_old, active=active, attempted=attempted):
                calls, _ = self.rollback_case(had_old=had_old, active=active, attempted=attempted, existed=had_old)
                self.assertEqual("systemctl:restart antapp-linkd\n" in calls, had_old and active and attempted)
                if had_old and active and attempted:
                    self.assertIn("restart-config:old configuration", calls)

    def test_failed_install_restores_original_config(self):
        self.rollback_case()

    def test_install_failure_restores_previous_binary(self):
        self.rollback_case(attempted=False)

    def test_failed_install_restores_derived_files_and_rules(self):
        calls, _ = self.rollback_case(derived=True)
        self.assertIn("new:down -c", calls)
        self.assertIn("old:up -c", calls)
        self.assertIn("systemctl:daemon-reload", calls)
        self.assertLess(calls.index("new:down"), calls.index("old:up"))
        self.assertIn("restart-config:old configuration", calls)

    def test_installer_and_updater_are_mutually_exclusive(self):
        import fcntl
        start = SOURCE.index('command -v flock')
        end = SOURCE.index('log "安装二进制', start)
        folder = self.folder / "lock-state"
        folder.mkdir()
        block = SOURCE[start:end].replace("/run/antapp-link", str(folder))
        script = 'die(){ echo "$*" >&2; exit 1; }\n' + block + '\nprintf acquired\n'
        with (folder / "update.lock").open("w") as held:
            fcntl.flock(held, fcntl.LOCK_EX)
            refused = subprocess.run(["bash", "-c", script], capture_output=True, text=True, encoding="utf-8")
            self.assertNotEqual(refused.returncode, 0)
            self.assertNotIn("acquired", refused.stdout)
        accepted = subprocess.run(["bash", "-c", script], capture_output=True, text=True, encoding="utf-8")
        self.assertEqual(accepted.returncode, 0, accepted.stderr)
        self.assertEqual(accepted.stdout, "acquired")
    def test_failed_config_restore_keeps_recovery_data(self):
        calls, _ = self.rollback_case(restore_failure=True)
        self.assertNotIn("systemctl:restart antapp-linkd\n", calls)

    def test_failed_old_service_restart_keeps_recovery_data(self):
        self.rollback_case(restart_failure=True)

    def test_unhealthy_old_service_keeps_recovery_data(self):
        self.rollback_case(health_failure=True)

    def test_failed_sysctl_restore_keeps_recovery_data(self):
        calls, _ = self.rollback_case(sysctl_failure=True)
        # sysctl 恢复失败只说明调优参数没写回去，不影响旧程序功能；资料保留，
        # 但仍要重启旧程序恢复业务，否则服务会一直停着。
        self.assertIn("systemctl:restart antapp-linkd\n", calls)
    def test_fresh_install_failure_removes_only_created_config(self):
        self.rollback_case(had_old=False, active=False, existed=False, derived=True)


class InstallEnvironmentTests(unittest.TestCase):
    def test_windows_has_actionable_wsl_error(self):
        with mock.patch.object(sys, "platform", "win32"):
            with self.assertRaisesRegex(RuntimeError, "WSL"):
                require_test_environment()

    def test_missing_dependencies_fail_before_running_tests(self):
        with mock.patch.object(sys, "platform", "linux"), mock.patch("shutil.which", return_value=None):
            with self.assertRaisesRegex(RuntimeError, "bash.*openssl"):
                require_test_environment()

    def test_linux_with_dependencies_is_accepted(self):
        with mock.patch.object(sys, "platform", "linux"), mock.patch("shutil.which", return_value="/usr/bin/tool"):
            require_test_environment()


class InstallVersionArgumentTests(unittest.TestCase):
    def parse(self, *args):
        start = SOURCE.index('while [[ $# -gt 0 ]]')
        end = SOURCE.index("\ndone", start) + len("\ndone")
        script = 'die(){ echo "$*" >&2; exit 1; }\n' + SOURCE[start:end] + '\nprintf "%s\n" "$RELEASE_TAG"\n'
        return subprocess.run(["bash", "-c", script, "test", *args], capture_output=True, text=True, encoding="utf-8")

    def test_version_tags_are_canonical(self):
        for value in ("1.2.3", "v1.2.3", "1.2.3-rc.1", "v1.2.3-rc.1", "1.2.3+build.4", "v1.2.3+build.4"):
            with self.subTest(value=value):
                result = self.parse("--version", value)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout.strip(), "v" + value.removeprefix("v"))

    def test_version_rejects_incomplete_or_unsafe_values(self):
        for args in (("--version",), ("--version", ""), ("--version", "v1junk"), ("--version", "1.2"), ("--version", "v1.2.3/path"), ("--version", "v1.2.3 "), ("--version", "vv1.2.3"), ("--version", "1.2.3-"), ("--version", "1.2.3+"), ("--version", "v1.2.3\n")):
            with self.subTest(args=args):
                self.assertNotEqual(self.parse(*args).returncode, 0)

    def test_canonical_tag_matches_signed_metadata(self):
        signed = InstallReleaseTests("test_valid_signature")
        signed.setUp()
        self.addCleanup(signed.doCleanups)
        for version in ("1.2.3", "1.2.3-rc.1", "1.2.3+build.4"):
            signed.manifest["version"] = version
            signed.manifest["server"]["url"] = "https://github.com/antapp-cc/antapp-link/releases/download/v" + version + "/antapp-linkd"
            signed.sign()
            for value in (version, "v" + version):
                with self.subTest(value=value):
                    result = self.parse("--version", value)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    signed.verify(True, result.stdout.strip(), "metadata")


if __name__ == "__main__":
    sys.stderr.reconfigure(encoding="utf-8")
    try:
        require_test_environment()
    except RuntimeError as exc:
        raise SystemExit(str(exc))
    unittest.main()
