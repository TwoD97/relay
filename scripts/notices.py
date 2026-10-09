#!/usr/bin/env python3
"""Collect license notices for dependencies actually distributed in the bundle."""
import json
import os
from pathlib import Path
import subprocess

root = Path(__file__).resolve().parent.parent
os.chdir(root)
output = ["Relay third-party license notices\n", "Targets: linux/amd64, linux/arm64, windows/amd64\n"]

def include(name, version, directory):
    directory = Path(directory)
    files = sorted({p for glob in ("LICENSE*", "LICENCE*", "COPYING*", "NOTICE*") for p in directory.glob(glob) if p.is_file()})
    output.append(f"\n{'=' * 72}\n{name} {version}\n")
    for path in files:
        output.append(f"\n--- {path.name} ---\n{path.read_bytes().decode('utf-8')}\n")
    if not files:
        raise SystemExit(f"No license notice found for {name}; inspect before packaging")

go = os.environ.get("RELAY_GO", "go")
# Go's paths and JSON use UTF-8 independently of the Windows ANSI code page.
goroot = Path(subprocess.check_output([go, "env", "GOROOT"], text=True, encoding="utf-8").strip())
go_version = subprocess.check_output([go, "version"], text=True, encoding="utf-8").strip()
include("Go standard library", go_version, goroot)
modules = {}
for platform, arch in (("linux", "amd64"), ("linux", "arm64"), ("windows", "amd64")):
    env = {**os.environ, "GOOS": platform, "GOARCH": arch, "CGO_ENABLED": "0"}
    stream = subprocess.check_output([go, "list", "-deps", "-json", "./cmd/relay"], text=True, encoding="utf-8", env=env)
    decoder = json.JSONDecoder()
    while stream.strip():
        package, end = decoder.raw_decode(stream.lstrip())
        stream = stream.lstrip()[end:]
        module = package.get("Module", {})
        if module and not module.get("Main"):
            modules[module["Path"]] = module
for name, module in sorted(modules.items()):
    include(name, module["Version"], module.get("Replace", module)["Dir"])

lock = json.loads((root / "web/package-lock.json").read_text(encoding="utf-8"))
for path, package in sorted(lock["packages"].items()):
    if not path or package.get("dev") or package.get("devOptional"):
        continue
    include(path.removeprefix("node_modules/"), package["version"], root / "web" / path)

(root / "dist").mkdir(exist_ok=True)
(root / "dist/THIRD_PARTY_NOTICES.txt").write_text("".join(output), encoding="utf-8", newline="\n")
