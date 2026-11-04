#!/usr/bin/env python3
"""Build the optional GPL connector, notices and offline corresponding source.

No personal account or network connector is started by this packaging script.
Go may download the pinned source dependencies, like the main release build.
"""
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parent.parent
MODULE = ROOT / "connectors/whatsapp-personal"
GO = os.environ.get("RELAY_GO", "go")
TARGETS = (("linux", "amd64"), ("linux", "arm64"), ("windows", "amd64"))


def command(args, **kwargs):
    return subprocess.check_output(args, text=True, encoding="utf-8", **kwargs)


def objects(stream):
    decoder = json.JSONDecoder()
    while stream.strip():
        value, end = decoder.raw_decode(stream.lstrip())
        stream = stream.lstrip()[end:]
        yield value


def license_files(directory):
    return sorted({p for pattern in ("LICENSE*", "LICENCE*", "COPYING*", "NOTICE*", "AUTHORS*")
                   for p in directory.glob(pattern) if p.is_file()})


def build(destination):
    destination.mkdir(parents=True, exist_ok=True)
    version = command([GO, "version"]).strip()
    if "go1.27.1 " not in version:
        raise SystemExit("The optional connector release requires Go 1.27.1")
    goroot = Path(command([GO, "env", "GOROOT"]).strip())
    modules = {}
    with tempfile.TemporaryDirectory(prefix="relay-whatsapp-build-") as temporary:
        temporary = Path(temporary)
        source = temporary / "relay-whatsapp-personal-source"
        source.mkdir()
        for path in sorted(MODULE.rglob("*")):
            if path.is_file() and (path.suffix == ".go" or path.relative_to(MODULE).as_posix() in
                                 ("go.mod", "go.sum", "LICENSE", "README.md", "build.sh")):
                target = source / path.relative_to(MODULE)
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(path, target)
                target.chmod(0o755 if path.name == "build.sh" else 0o644)
        subprocess.run([GO, "mod", "vendor", "-o", str(source / "vendor")], cwd=MODULE, check=True)
        hashes = []
        for system, architecture in TARGETS:
            environment = {**os.environ, "GOOS": system, "GOARCH": architecture, "CGO_ENABLED": "0"}
            name = f"relay-whatsapp-personal-{system}-{architecture}" + (".exe" if system == "windows" else "")
            for package in objects(command([GO, "list", "-deps", "-json", "."], cwd=MODULE, env=environment)):
                module = package.get("Module", {})
                if module and not module.get("Main"):
                    modules[module["Path"]] = module
            target = temporary / name
            # Compile the provided corresponding source, rather than a second,
            # potentially different package tree in the module cache.
            subprocess.run([GO, "build", "-mod=vendor", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", str(target), "."],
                           cwd=source, env=environment, check=True)
            hashes.append(f"{hashlib.sha256(target.read_bytes()).hexdigest()}  {name}\n")
        notices = ["Optional Relay personal WhatsApp connector\n",
                   "This separate executable is distributed under GPL-3.0-only.\n",
                   "It includes whatsmeow (MPL-2.0), libsignal (GPL-3.0), and other dependencies listed below.\n",
                   "Relay's core does not import or link these connector dependencies.\n",
                   "Corresponding source and an offline build script accompany the executable.\n",
                   f"Build toolchain: {version}\n", "\nGo standard library license\n", (goroot / "LICENSE").read_text(encoding="utf-8")]
        source_licenses = source / "dependency-licenses"
        source_licenses.mkdir()
        shutil.copyfile(goroot / "LICENSE", source_licenses / "Go-LICENSE")
        for name, module in sorted(modules.items()):
            directory = Path(module.get("Replace", module)["Dir"])
            files = license_files(directory)
            if not files:
                raise SystemExit(f"Missing license text for {name}; packaging refused")
            notices.append(f"\n{'=' * 72}\n{name} {module['Version']}\n")
            folder = source_licenses / (name.replace("/", "_") + "@" + module["Version"])
            folder.mkdir()
            for path in files:
                notices.append(f"\n--- {path.name} ---\n{path.read_bytes().decode('utf-8')}\n")
                shutil.copyfile(path, folder / path.name)
        notice_text = "".join(notices)
        (source / "NOTICES.txt").write_text(notice_text, encoding="utf-8", newline="\n")
        (source / "BUILD-INFO.txt").write_text(version + "\nCGO_ENABLED=0; -mod=vendor -trimpath -buildvcs=false -ldflags='-s -w'\n", encoding="utf-8")
        archive = temporary / "relay-whatsapp-personal-source.tar.gz"
        with archive.open("wb") as output, gzip.GzipFile(filename="", mode="wb", fileobj=output, mtime=0) as compressed:
            with tarfile.open(fileobj=compressed, mode="w") as tar:
                for path in sorted(source.rglob("*")):
                    if not path.is_file():
                        continue
                    data = path.read_bytes()
                    info = tarfile.TarInfo("relay-whatsapp-personal-source/" + path.relative_to(source).as_posix())
                    info.size = len(data)
                    info.mode = 0o755 if path.name == "build.sh" else 0o644
                    info.mtime = 0
                    tar.addfile(info, io.BytesIO(data))
        for line in hashes:
            name = line.split()[1]
            shutil.copyfile(temporary / name, destination / name)
            (destination / name).chmod(0o755)
        shutil.copyfile(MODULE / "LICENSE", destination / "WHATSAPP_PERSONAL_LICENSE.txt")
        (destination / "WHATSAPP_PERSONAL_NOTICES.txt").write_text(notice_text, encoding="utf-8", newline="\n")
        shutil.copyfile(archive, destination / archive.name)
        (destination / "SHA256SUMS.whatsapp").write_text("".join(hashes), encoding="ascii", newline="\n")
    print("Built optional personal WhatsApp connector for all three targets, with licenses and corresponding source.")


if __name__ == "__main__":
    build(Path(sys.argv[1] if len(sys.argv) > 1 else ROOT / "dist").resolve())
