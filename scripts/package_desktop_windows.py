#!/usr/bin/env python3
"""Package an already compiled Windows desktop without installing or launching it."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import struct
import tempfile
import zipfile


BINARIES = {"relay-linux-amd64", "relay-linux-arm64", "relay-controller-windows-amd64.exe"}
RUNTIME_FILES = sorted(BINARIES) + [
    "SHA256SUMS", "SHA256SUMS.windows", "VERSION", "LICENSE", "NOTICE",
    "THIRD_PARTY_NOTICES.md", "THIRD_PARTY_NOTICES.txt", "README.md",
    "docs/ACTIVITY.md", "docs/CONTRACT.md", "docs/SHARED_CONTEXT.md", "docs/VALIDATION.md",
]
MAX_FILE_BYTES = 128 << 20


def plain_path(path, directory=False):
    """Reject symlinks/junctions before reading inputs or publishing outputs."""
    path = Path(os.path.abspath(path))
    for part in (path, *path.parents):
        info = part.lstat()
        if stat.S_ISLNK(info.st_mode) or getattr(info, "st_file_attributes", 0) & 0x400:
            raise ValueError(f"Symbolic links and junctions are not package inputs: {part}")
        if part == path:
            valid = stat.S_ISDIR(info.st_mode) if directory else stat.S_ISREG(info.st_mode)
            if not valid:
                raise ValueError(f"Expected a plain {'directory' if directory else 'file'}: {part}")
        elif not stat.S_ISDIR(info.st_mode):
            raise ValueError(f"Expected a plain parent directory: {part}")
    return path


def read_input(path):
    path = plain_path(path)
    before = path.stat()
    if before.st_size > MAX_FILE_BYTES:
        raise ValueError(f"Package input exceeds its size limit: {path.name}")
    descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_BINARY", 0))
    with os.fdopen(descriptor, "rb") as stream:
        opened = os.fstat(stream.fileno())
        if (opened.st_dev, opened.st_ino) != (before.st_dev, before.st_ino) or not stat.S_ISREG(opened.st_mode):
            raise ValueError("A package input changed while opening it")
        data = stream.read(MAX_FILE_BYTES + 1)
    if len(data) > MAX_FILE_BYTES:
        raise ValueError(f"Package input exceeds its size limit: {path.name}")
    return data


def digest(data):
    return hashlib.sha256(data).hexdigest()


def validate_gui_executable(data):
    if len(data) < 64 or data[:2] != b"MZ":
        raise ValueError("The compiled desktop is not a Windows executable")
    offset = struct.unpack_from("<I", data, 0x3c)[0]
    if offset > len(data) - 96 or data[offset:offset + 4] != b"PE\0\0":
        raise ValueError("The compiled desktop has an invalid PE header")
    if struct.unpack_from("<H", data, offset + 4)[0] != 0x8664:
        raise ValueError("The portable desktop must be Windows x64")
    if struct.unpack_from("<H", data, offset + 24 + 68)[0] != 2:
        raise ValueError("The portable desktop must use the Windows GUI subsystem")


def collect_payload(stage, readme):
    stage = plain_path(stage, directory=True)
    marker = json.loads(read_input(stage / ".relay-desktop-stage.json").decode("utf-8-sig"))
    if marker.get("format") != 1 or marker.get("application") != "relay-desktop":
        raise ValueError("The build stage is not owned by Relay")
    runtime = stage / "resources/runtime"
    files = {"runtime/" + name: read_input(runtime / name) for name in RUNTIME_FILES}
    version = files["runtime/VERSION"].decode("utf-8").strip()
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._+-]{0,100}", version):
        raise ValueError("Invalid runtime bundle version")
    expected = {}
    for name, allowed in [("SHA256SUMS", BINARIES - {"relay-controller-windows-amd64.exe"}),
                          ("SHA256SUMS.windows", {"relay-controller-windows-amd64.exe"})]:
        entries = {}
        for line in files["runtime/" + name].decode("utf-8").splitlines():
            match = re.fullmatch(r"([0-9a-fA-F]{64})\s+\*?([A-Za-z0-9._-]+)", line)
            if not match or match[2] in entries:
                raise ValueError("Invalid or duplicate runtime checksum entry")
            entries[match[2]] = match[1].lower()
        if set(entries) != allowed:
            raise ValueError("The runtime checksum manifest has unexpected or missing artifacts")
        expected.update(entries)
    for name, checksum in expected.items():
        if digest(files["runtime/" + name]) != checksum:
            raise ValueError("Runtime checksum mismatch: " + name)
    desktop = read_input(stage / "target/release/relay-desktop.exe")
    validate_gui_executable(desktop)
    files["relay-desktop.exe"] = desktop
    notices = read_input(stage / "resources/RUST_THIRD_PARTY_NOTICES.txt")
    if "Target: x86_64-pc-windows-msvc" not in notices.decode("utf-8"):
        raise ValueError("Native Windows dependency notices are required")
    files["RUST_THIRD_PARTY_NOTICES.txt"] = notices
    files["README.txt"] = read_input(readme)
    for name in ["LICENSE", "NOTICE", "THIRD_PARTY_NOTICES.md"]:
        files[name] = files["runtime/" + name]
    files["SHA256SUMS"] = "".join(f"{digest(data)}  {name}\n" for name, data in sorted(files.items())).encode("utf-8")
    return version, files


def publish_bytes(path, data, replace):
    if replace:
        fd, temporary = tempfile.mkstemp(prefix=".relay-package-", dir=path.parent)
        try:
            with os.fdopen(fd, "wb") as stream:
                stream.write(data)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temporary, path)
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)
    else:
        with path.open("xb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())


def package(stage, output, readme, replace=False):
    output = Path(os.path.abspath(output))
    if output.suffix.lower() != ".zip":
        raise ValueError("Portable package output must be a .zip file")
    plain_path(output.parent, directory=True)
    sidecar = output.with_name(output.name + ".sha256")
    for path in [output, sidecar]:
        if path.exists() or path.is_symlink():
            plain_path(path)
            if not replace:
                raise ValueError("Output already exists; explicitly use --replace for generated build artifacts")
    version, files = collect_payload(stage, readme)
    fd, temporary = tempfile.mkstemp(prefix=".relay-portable-", suffix=".zip", dir=output.parent)
    os.close(fd)
    try:
        with zipfile.ZipFile(temporary, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
            for name, data in sorted(files.items()):
                entry = zipfile.ZipInfo("Relay/" + name, date_time=(1980, 1, 1, 0, 0, 0))
                entry.create_system = 0
                entry.compress_type = zipfile.ZIP_DEFLATED
                archive.writestr(entry, data, compresslevel=9)
        with zipfile.ZipFile(temporary) as archive:
            if archive.testzip() is not None or set(archive.namelist()) != {"Relay/" + name for name in files}:
                raise ValueError("Portable archive failed integrity verification")
            for name, data in files.items():
                if archive.read("Relay/" + name) != data:
                    raise ValueError("Portable archive contents changed")
        data = Path(temporary).read_bytes()
        checksum = digest(data)
        publish_bytes(output, data, replace)
        publish_bytes(sidecar, f"{checksum}  {output.name}\n".encode("utf-8"), replace)
        return {"version": version, "archive": str(output), "bytes": len(data), "sha256": checksum,
                "files": len(files), "installed": False, "launched": False}
    finally:
        os.unlink(temporary)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--stage", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--readme", type=Path, default=Path(__file__).resolve().parent.parent / "desktop/PORTABLE.md")
    parser.add_argument("--replace", action="store_true", help="replace generated build outputs after successful validation")
    args = parser.parse_args()
    print(json.dumps(package(args.stage, args.output, args.readme, args.replace), indent=2))
