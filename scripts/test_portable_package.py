#!/usr/bin/env python3
"""Portable packaging regressions; only disposable files, no app processes."""
import base64
import hashlib
import json
import os
from pathlib import Path
import shutil
import struct
import subprocess
import tempfile
import unittest
import zipfile

from package_desktop_windows import BINARIES, RUNTIME_FILES, package


class PortablePackageTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="relay portable ")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.stage = self.root / "stage with spaces"
        self.runtime = self.stage / "resources/runtime"
        self.runtime.mkdir(parents=True)
        self.readme = self.root / "portable readme.txt"
        self.readme.write_text("Extract the full folder. Fixture text: ✓\n", encoding="utf-8")
        self.output = self.root / "Relay portable.zip"
        (self.stage / ".relay-desktop-stage.json").write_text(json.dumps({"format": 1, "application": "relay-desktop"}), encoding="utf-8-sig")
        for name in RUNTIME_FILES:
            path = self.runtime / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(("public fixture " + name + "\n").encode())
        (self.runtime / "VERSION").write_text("0.1.0-test.portable\n", encoding="utf-8")
        for manifest, names in [("SHA256SUMS", sorted(BINARIES - {"relay-controller-windows-amd64.exe"})),
                                ("SHA256SUMS.windows", ["relay-controller-windows-amd64.exe"])]:
            (self.runtime / manifest).write_text("".join(hashlib.sha256((self.runtime / name).read_bytes()).hexdigest() + "  " + name + "\n" for name in names), encoding="utf-8")
        (self.stage / "resources/RUST_THIRD_PARTY_NOTICES.txt").write_text("Target: x86_64-pc-windows-msvc\nFixture license ✓\n", encoding="utf-8")
        pe = bytearray(256)
        pe[:2] = b"MZ"
        struct.pack_into("<I", pe, 0x3c, 128)
        pe[128:132] = b"PE\0\0"
        struct.pack_into("<H", pe, 132, 0x8664)
        struct.pack_into("<H", pe, 128 + 24 + 68, 2)
        self.executable = self.stage / "target/release/relay-desktop.exe"
        self.executable.parent.mkdir(parents=True)
        self.executable.write_bytes(pe)

    def build(self, **kwargs):
        return package(self.stage, self.output, self.readme, **kwargs)

    def test_complete_archive_preserves_hashes_notices_and_excludes_state(self):
        (self.runtime / "authentication.json").write_text("PRIVATE-FIXTURE-MUST-NOT-SHIP")
        (self.stage / "resources/private.log").write_text("PRIVATE-FIXTURE-MUST-NOT-SHIP")
        result = self.build()
        self.assertEqual(result["version"], "0.1.0-test.portable")
        self.assertFalse(result["installed"])
        self.assertFalse(result["launched"])
        self.assertEqual(result["sha256"], hashlib.sha256(self.output.read_bytes()).hexdigest())
        self.assertEqual(self.output.with_name(self.output.name + ".sha256").read_text(), result["sha256"] + "  " + self.output.name + "\n")
        with zipfile.ZipFile(self.output) as archive:
            self.assertIsNone(archive.testzip())
            names = set(archive.namelist())
            checksums = archive.read("Relay/SHA256SUMS").decode().splitlines()
            self.assertEqual(len(checksums), len(names) - 1)
            for line in checksums:
                checksum, name = line.split("  ", 1)
                self.assertEqual(hashlib.sha256(archive.read("Relay/" + name)).hexdigest(), checksum)
            for name in RUNTIME_FILES:
                self.assertEqual(archive.read("Relay/runtime/" + name), (self.runtime / name).read_bytes())
            self.assertEqual(archive.read("Relay/README.txt"), self.readme.read_bytes())
            self.assertEqual(archive.read("Relay/relay-desktop.exe"), self.executable.read_bytes())
            self.assertNotIn("Relay/runtime/authentication.json", names)
            self.assertNotIn("Relay/private.log", names)
            self.assertFalse(any(b"PRIVATE-FIXTURE-MUST-NOT-SHIP" in archive.read(name) for name in names))

    def test_tampered_payload_preserves_existing_archive_and_checksum(self):
        self.build()
        before = self.output.read_bytes()
        sidecar = self.output.with_name(self.output.name + ".sha256")
        checksum = sidecar.read_bytes()
        (self.runtime / "relay-controller-windows-amd64.exe").write_bytes(b"tampered")
        with self.assertRaisesRegex(ValueError, "checksum mismatch"):
            self.build(replace=True)
        self.assertEqual(self.output.read_bytes(), before)
        self.assertEqual(sidecar.read_bytes(), checksum)

    def test_existing_archive_requires_explicit_replacement(self):
        self.output.write_bytes(b"another package")
        with self.assertRaisesRegex(ValueError, "already exists"):
            self.build()
        self.assertEqual(self.output.read_bytes(), b"another package")

    def test_missing_required_components_never_publish_partial_package(self):
        for path in [self.runtime / "relay-controller-windows-amd64.exe", self.runtime / "docs/ACTIVITY.md", self.runtime / "NOTICE", self.stage / "resources/RUST_THIRD_PARTY_NOTICES.txt"]:
            with self.subTest(path=path.name):
                contents = path.read_bytes()
                path.unlink()
                with self.assertRaises(FileNotFoundError):
                    self.build()
                self.assertFalse(self.output.exists())
                path.write_bytes(contents)

    def test_manifest_injection_and_duplicate_entries_are_rejected(self):
        path = self.runtime / "SHA256SUMS.windows"
        original = path.read_text()
        for text in [original + original, original + "0" * 64 + "  ../private\n", original.replace("relay-controller-windows-amd64.exe", "unexpected.exe")]:
            with self.subTest(manifest=text):
                path.write_text(text)
                with self.assertRaisesRegex(ValueError, "checksum"):
                    self.build()
                self.assertFalse(self.output.exists())

    def test_wrong_notice_target_and_foreign_stage_are_rejected(self):
        notices = self.stage / "resources/RUST_THIRD_PARTY_NOTICES.txt"
        notices.write_text("Target: x86_64-unknown-linux-gnu\n")
        with self.assertRaisesRegex(ValueError, "Windows dependency notices"):
            self.build()
        (self.stage / ".relay-desktop-stage.json").write_text('{"format":1,"application":"unrelated"}')
        with self.assertRaisesRegex(ValueError, "not owned"):
            self.build()

    def test_console_executable_cannot_ship_as_desktop(self):
        pe = bytearray(self.executable.read_bytes())
        struct.pack_into("<H", pe, 128 + 24 + 68, 3)
        self.executable.write_bytes(pe)
        with self.assertRaisesRegex(ValueError, "GUI subsystem"):
            self.build()

    def test_input_symlink_rejected_without_touching_target(self):
        target = self.root / "unrelated public fixture"
        target.write_bytes(b"sentinel")
        path = self.runtime / "NOTICE"
        path.unlink()
        try:
            path.symlink_to(target)
        except OSError as error:
            self.skipTest("Creating symlinks is unavailable: " + str(error))
        with self.assertRaisesRegex(ValueError, "Symbolic links"):
            self.build()
        self.assertEqual(target.read_bytes(), b"sentinel")
        self.assertFalse(self.output.exists())

    @unittest.skipUnless(os.name == "nt", "Windows junction regression")
    def test_windows_directory_junction_is_rejected(self):
        target = self.root / "other docs"
        docs = self.runtime / "docs"
        shutil.move(docs, target)
        quote = lambda value: "'" + str(value).replace("'", "''") + "'"
        script = "$ErrorActionPreference='Stop'; New-Item -ItemType Junction -Path " + quote(docs) + " -Target " + quote(target) + " | Out-Null"
        encoded = base64.b64encode(script.encode("utf-16-le")).decode()
        subprocess.run(["powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", encoded], check=True, capture_output=True)
        try:
            with self.assertRaisesRegex(ValueError, "junctions"):
                self.build()
            self.assertTrue((target / "ACTIVITY.md").is_file())
            self.assertFalse(self.output.exists())
        finally:
            os.rmdir(docs)


if __name__ == "__main__":
    unittest.main()
