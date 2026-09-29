"""Linux installer tests using only the Python standard library.

Plan tests run real read-only checks. Installation tests redirect every output
into TemporaryDirectory and replace privileged/systemd operations with doubles.
No real accounts, service units or services are created or changed.
"""
import hashlib
import json
import os
from pathlib import Path
import pwd
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("install.sh").resolve()
BINARY = Path(os.environ.get("INSTALL_TEST_BINARY", "dist/rules-mcp-linux-amd64")).resolve()


@unittest.skipUnless(sys.platform == "linux", "Debian installer tests require Linux")
class InstallerTests(unittest.TestCase):
    def setUp(self):
        # PrivateTmp deliberately hides /tmp and /var/tmp from the service.
        self.temp = tempfile.TemporaryDirectory(prefix="rules-mcp-installer-", dir=Path.home())
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / "rules"
        self.repo.mkdir()
        self.user = pwd.getpwuid(os.getuid()).pw_name
        self.name = "rules-mcp-test-" + self.root.name.rsplit("-", 1)[-1]
        # Hosted runners may make /opt writable for tool installation. Use an
        # existing root-managed parent for planned outputs; never write there.
        self.install_dir = Path("/etc") / self.name / "bin"
        self.config_dir = Path("/etc") / self.name / "config"
        for args in [
            ["init", "--initial-branch=dev"],
            ["config", "user.name", "Installer Test"],
            ["config", "user.email", "installer@example.invalid"],
            ["remote", "add", "origin", str(self.root / "remote.git")],
        ]:
            self.git(*args)
        (self.repo / "json").mkdir()
        (self.repo / "sample.yaml").write_text("payload:\n  - DOMAIN,example.com\n")
        (self.repo / "json/sample.json").write_text('{"version":1,"rules":[{"domain":["example.com"]}]}\n')
        self.git("add", "sample.yaml", "json/sample.json")
        self.git("-c", "commit.gpgSign=false", "commit", "-m", "fixture")

    def git(self, *args):
        return subprocess.run(["git", "-C", str(self.repo), *args], check=True,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True).stdout.strip()

    def args(self):
        return ["--plan", "--user", self.user, "--repo", str(self.repo),
                "--binary", str(BINARY), "--install-dir", str(self.install_dir),
                "--config-dir", str(self.config_dir), "--service-name", self.name]

    def invoke(self, *extra):
        return subprocess.run(["bash", str(SCRIPT), *self.args(), *extra],
                              input="", text=True, capture_output=True)

    def snapshot(self):
        return {str(p.relative_to(self.repo)): hashlib.sha256(p.read_bytes()).hexdigest()
                for p in self.repo.rglob("*") if p.is_file()}

    def test_plan_is_read_only_and_renders_parameters(self):
        before = self.snapshot()
        p = self.invoke("--listen", "[::1]:9000", "--publish-branch", "-", "--timeout", "60", "--start", "no")
        self.assertEqual(p.returncode, 0, p.stderr)
        config = json.loads(p.stdout.split("将生成的 config.json：\n")[1].split("\n将生成的 systemd unit：")[0])
        self.assertEqual(config, dict(listen="[::1]:9000", repository=str(self.repo), branch="dev",
                                      remote="origin", publish_branch="", timeout_seconds=60))
        self.assertIn("User=" + self.user, p.stdout)
        self.assertIn("ProtectHome=read-only", p.stdout)
        self.assertIn("ReadWritePaths=" + str(self.repo), p.stdout)
        self.assertIn("TimeoutStopSec=90", p.stdout)
        self.assertEqual(before, self.snapshot(), "plan changed the Git checkout")
        self.assertFalse(self.install_dir.exists())
        self.assertFalse(self.config_dir.exists())

    def test_rejects_invalid_inputs_before_installation(self):
        cases = [("--user", "root"), ("--branch", "master"), ("--branch", "main"),
                 ("--listen", "0.0.0.0:8787"), ("--listen", "127.0.0.1:80"),
                 ("--listen", "127.0.0.1:65536"), ("--timeout", "0"),
                 ("--timeout", "601"), ("--service-name", "bad%name"),
                 ("--install-dir", str(self.repo)), ("--install-dir", "/opt/../etc/bad"),
                 ("--repo", "/tmp/rules"), ("--repo", "/var/tmp/rules"),
                 ("--config-dir", "/etc/bad name"), ("--start", "maybe")]
        for case in cases:
            with self.subTest(case=case):
                self.assertNotEqual(self.invoke(*case).returncode, 0)

    def test_dirty_wrong_branch_or_missing_remote_fail(self):
        self.assertNotEqual(self.invoke("--branch", "other").returncode, 0)
        self.assertNotEqual(self.invoke("--remote", "missing").returncode, 0)
        (self.repo / "unrelated.txt").write_text("preserve")
        self.assertNotEqual(self.invoke().returncode, 0)
        self.assertEqual((self.repo / "unrelated.txt").read_text(), "preserve")

    def test_missing_explicit_author_fails(self):
        self.git("config", "user.email", "")
        self.assertNotEqual(self.invoke().returncode, 0)

    def test_existing_output_is_not_overwritten(self):
        directory = self.root / "existing"
        directory.mkdir()
        target = directory / "config.json"
        target.write_text("keep this config")
        p = self.invoke("--config-dir", str(directory))
        self.assertNotEqual(p.returncode, 0)
        self.assertEqual(target.read_text(), "keep this config")

    def test_symlink_repository_rejected(self):
        link = self.root / "link"
        link.symlink_to(self.repo, target_is_directory=True)
        self.assertNotEqual(self.invoke("--repo", str(link)).returncode, 0)

    def test_generated_unit_syntax(self):
        p = self.invoke()
        self.assertEqual(p.returncode, 0, p.stderr)
        unit = p.stdout.split("将生成的 systemd unit：\n")[1].split("\n检查仅覆盖")[0]
        # Verification checks ExecStart existence, so point the executable to the
        # real test binary while retaining all generated options and directives.
        unit = unit.replace(str(self.install_dir / "rules-mcp"), str(BINARY))
        unit_file = self.root / (self.name + ".service")
        unit_file.write_text(unit)
        result = subprocess.run(["systemd-analyze", "verify", str(unit_file)], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)

    def simulated_install(self, start="no", verify_fails=False):
        env = os.environ | {"TEST_ROOT": str(self.root), "TEST_REPO": str(self.repo),
                           "TEST_BINARY": str(BINARY), "TEST_SCRIPT": str(SCRIPT),
                           "TEST_USER": self.user, "TEST_START": start,
                           "VERIFY_FAILS": "yes" if verify_fails else "no"}
        program = r'''
set -euo pipefail
source "$TEST_SCRIPT"
defaults
RUN_USER="$TEST_USER"; RUN_UID=$(id -u); RUN_GROUP=$(id -g); RUN_HOME="$HOME"
REPO="$TEST_REPO"; BINARY="$TEST_BINARY"; YES=yes; START="$TEST_START"
INSTALL_DIR="$TEST_ROOT/installed"; CONFIG_DIR="$TEST_ROOT/config"
INSTALLED_BINARY="$INSTALL_DIR/rules-mcp"; CONFIG_FILE="$CONFIG_DIR/config.json"
UNIT_FILE="$TEST_ROOT/rules-mcp.service"
require_install_host() { :; }
managed_directory() { mkdir -p -- "$1"; }
as_user() { "$@"; }
chown() { [[ "${@: -1}" == "$TEST_ROOT/"* ]]; }
mktemp() { /usr/bin/mktemp -d "$TEST_ROOT/staging.XXXXXXXX"; }
install() {
  local -a args=()
  while (($#)); do
    case "$1" in -o|-g) shift 2 ;; *) args+=("$1"); shift ;; esac
  done
  /usr/bin/install "${args[@]}"
}
systemctl() {
  printf '%s\n' "$*" >> "$TEST_ROOT/systemctl.log"
  if [[ "$1" == show ]]; then printf 'not-found\n'; fi
}
systemd-analyze() { [[ "$VERIFY_FAILS" != yes ]]; }
install_service
'''
        return subprocess.run(["bash", "-c", program], env=env, capture_output=True, text=True)

    def test_install_renders_and_checks_without_start(self):
        p = self.simulated_install()
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(json.loads((self.root / "config/config.json").read_text())["repository"], str(self.repo))
        self.assertTrue(os.access(self.root / "installed/rules-mcp", os.X_OK))
        log = (self.root / "systemctl.log").read_text()
        self.assertIn("daemon-reload", log)
        self.assertNotIn("enable", log)
        self.assertTrue((self.root / "rules-mcp.service").exists())

    def test_start_is_explicit_and_service_is_verified(self):
        p = self.simulated_install(start="yes")
        self.assertEqual(p.returncode, 0, p.stderr)
        log = (self.root / "systemctl.log").read_text()
        self.assertIn("enable --now rules-mcp.service", log)
        self.assertIn("is-active --quiet rules-mcp.service", log)

    def test_failed_unit_validation_does_not_start_or_erase(self):
        p = self.simulated_install(verify_fails=True)
        self.assertNotEqual(p.returncode, 0)
        self.assertTrue((self.root / "installed/rules-mcp").exists())
        self.assertTrue((self.root / "config/config.json").exists())
        self.assertFalse((self.root / "rules-mcp.service").exists())
        self.assertNotIn("enable", (self.root / "systemctl.log").read_text())


if __name__ == "__main__":
    unittest.main(verbosity=2)
