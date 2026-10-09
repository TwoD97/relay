#!/usr/bin/env python3
"""Behavioral installer checks; all paths and binaries are disposable fixtures."""

import hashlib
import os
import pathlib
import platform
import shutil
import subprocess
import tempfile
import textwrap
import unittest


@unittest.skipUnless(
    platform.system() == "Linux" and platform.machine() in ("x86_64", "aarch64", "arm64"),
    "the installer supports Linux/WSL amd64 and arm64",
)
class InstallTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="relay-install-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = pathlib.Path(self.temporary.name)
        self.bundle = self.root / "bundle with spaces"
        self.bundle.mkdir()
        shutil.copyfile(pathlib.Path(__file__).with_name("install.sh"), self.bundle / "install.sh")
        self.install_root = self.root / "installed releases"
        self.binary_dir = self.root / "local bin"
        self.environment = dict(
            os.environ,
            RELAY_INSTALL_ROOT=str(self.install_root),
            RELAY_INSTALL_BIN=str(self.binary_dir),
        )
        self.write_bundle("test-v1")

    def write_bundle(self, version):
        checksums = []
        for architecture in ("amd64", "arm64"):
            name = "relay-linux-" + architecture
            content = ("#!/bin/sh\nprintf '%s\\n' '" + version + "'\n").encode()
            (self.bundle / name).write_bytes(content)
            checksums.append(hashlib.sha256(content).hexdigest() + "  " + name + "\n")
        (self.bundle / "SHA256SUMS").write_text("".join(checksums))
        (self.bundle / "VERSION").write_text(version + "\n")

    def install(self, *, environment=None, expected=0):
        result = subprocess.run(
            ["bash", str(self.bundle / "install.sh")],
            cwd=self.root,
            env=self.environment if environment is None else environment,
            capture_output=True,
            text=True,
            timeout=20,
        )
        if expected == 0:
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        return result

    def assert_active(self, version, binary_dir=None):
        link = (binary_dir or self.binary_dir) / "relay"
        self.assertTrue(link.is_symlink())
        self.assertTrue(link.exists(), f"broken link: {link.readlink()}")
        result = subprocess.run([str(link)], check=True, capture_output=True, text=True)
        self.assertEqual(result.stdout, version + "\n")

    def cp_wrapper(self, source):
        wrapper_dir = self.root / "wrappers"
        wrapper_dir.mkdir()
        wrapper = wrapper_dir / "cp"
        real_cp = shutil.which("cp")
        self.assertIsNotNone(real_cp)
        wrapper.write_text(
            "#!/usr/bin/env python3\n"
            "import os,pathlib,subprocess,sys,time\n"
            f"subprocess.run([{real_cp!r},*sys.argv[1:]],check=True)\n"
            + textwrap.dedent(source)
        )
        wrapper.chmod(0o700)
        environment = dict(self.environment)
        environment["PATH"] = str(wrapper_dir) + os.pathsep + os.environ["PATH"]
        return environment

    def test_normal_install_and_idempotent_reinstall(self):
        self.install()
        self.assert_active("test-v1")
        before = (self.binary_dir / "relay").resolve().stat().st_ino
        self.install()
        self.assert_active("test-v1")
        self.assertEqual((self.binary_dir / "relay").resolve().stat().st_ino, before)
        self.assertEqual(
            sorted(p.name for p in (self.install_root / "releases").iterdir()), ["test-v1"]
        )
        self.assertEqual(sorted(p.name for p in self.binary_dir.iterdir()), ["relay"])

    def test_installed_bundle_retains_license_and_attribution_files(self):
        notices = ("LICENSE", "NOTICE", "THIRD_PARTY_NOTICES.md", "THIRD_PARTY_NOTICES.txt")
        for name in notices:
            (self.bundle / name).write_text("Attribution: " + name)
        self.install()
        release = (self.binary_dir / "relay").resolve().parent
        for name in notices:
            self.assertEqual((release / name).read_text(), "Attribution: " + name)

    def test_symlink_to_directory_is_replaced_without_writing_through_it(self):
        unrelated = self.root / "unrelated"
        unrelated.mkdir()
        (unrelated / "keep").write_text("untouched")
        self.binary_dir.mkdir()
        (self.binary_dir / "relay").symlink_to(unrelated, target_is_directory=True)
        self.install()
        self.assert_active("test-v1")
        self.assertEqual(sorted(p.name for p in unrelated.iterdir()), ["keep"])
        self.assertEqual((unrelated / "keep").read_text(), "untouched")

    def test_directory_activation_is_refused_without_nesting(self):
        destination = self.binary_dir / "relay"
        destination.mkdir(parents=True)
        (destination / "keep").write_text("untouched")
        self.install(expected=1)
        self.assertTrue(destination.is_dir())
        self.assertEqual(sorted(p.name for p in destination.iterdir()), ["keep"])

    def test_relative_overrides_resolve_from_invoking_directory(self):
        environment = dict(
            self.environment,
            RELAY_INSTALL_ROOT="relative releases",
            RELAY_INSTALL_BIN="relative bin",
        )
        self.install(environment=environment)
        self.assert_active("test-v1", self.root / "relative bin")
        self.assertTrue((self.root / "relative releases/releases/test-v1").is_dir())
        self.assertFalse((self.bundle / "relative releases").exists())

    def test_changed_staged_bytes_do_not_activate_or_damage_previous_release(self):
        self.install()
        self.write_bundle("test-v2")
        environment = self.cp_wrapper("""
            destination=pathlib.Path(sys.argv[-1])
            with (destination/'relay-linux-amd64').open('ab') as output:
                output.write(b'changed-after-source-checksum')
        """)
        self.install(environment=environment, expected=1)
        self.assert_active("test-v1")
        self.assertFalse((self.install_root / "releases/test-v2").exists())
        self.assertEqual(list((self.install_root / "releases").glob(".install.*")), [])

    def test_version_collision_preserves_existing_release(self):
        self.install()
        original = (self.binary_dir / "relay").resolve().read_bytes()
        self.write_bundle("different-content")
        (self.bundle / "VERSION").write_text("test-v1\n")
        self.install(expected=1)
        self.assertEqual((self.binary_dir / "relay").resolve().read_bytes(), original)

    def test_release_symlink_is_refused(self):
        destination = self.install_root / "releases"
        destination.mkdir(parents=True)
        unrelated = self.root / "unrelated-release"
        unrelated.mkdir()
        (destination / "test-v1").symlink_to(unrelated, target_is_directory=True)
        self.install(expected=1)
        self.assertEqual(list(unrelated.iterdir()), [])
        self.assertFalse((self.binary_dir / "relay").exists())

    def test_manifest_requires_both_architectures(self):
        manifest = self.bundle / "SHA256SUMS"
        manifest.write_text(manifest.read_text().splitlines()[0] + "\n")
        (self.bundle / "relay-linux-arm64").write_bytes(b"unverified bytes")
        self.install(expected=1)
        self.assertFalse((self.binary_dir / "relay").exists())

    def test_concurrent_installers_do_not_nest_staging_or_replace_winning_release(self):
        barrier = self.root / "copied"
        barrier.mkdir()
        environment = self.cp_wrapper("""
            barrier=pathlib.Path(os.environ['RELAY_TEST_BARRIER'])
            (barrier/str(os.getpid())).touch()
            deadline=time.monotonic()+10
            while len(list(barrier.iterdir()))<2:
                if time.monotonic()>deadline: raise SystemExit('copy barrier timed out')
                time.sleep(0.01)
        """)
        environment["RELAY_TEST_BARRIER"] = str(barrier)
        commands = []
        try:
            for _ in range(2):
                commands.append(subprocess.Popen(
                    ["bash", str(self.bundle / "install.sh")], cwd=self.root,
                    env=environment, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                ))
            for command in commands:
                stdout, stderr = command.communicate(timeout=20)
                self.assertEqual(command.returncode, 0, stdout + stderr)
        finally:
            for command in commands:
                if command.poll() is None:
                    command.kill()
                    command.communicate()
        self.assert_active("test-v1")
        self.assertEqual(sorted(p.name for p in (self.install_root / "releases").iterdir()), ["test-v1"])
        self.assertFalse(any((self.install_root / "releases/test-v1").glob(".install.*")))


if __name__ == "__main__":
    unittest.main()
