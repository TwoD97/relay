#!/usr/bin/env python3
"""Collect dependency license files and exact source locations for the desktop bundle."""
import json
import os
import argparse
import hashlib
from html.parser import HTMLParser
from pathlib import Path
import subprocess

root = Path(__file__).resolve().parent.parent
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--target", help="Cargo target triple; defaults to the host toolchain")
parser.add_argument("--output", type=Path, help="Destination notices file")
args = parser.parse_args()
cargo = os.environ.get("RELAY_CARGO", "cargo")
target = args.target or subprocess.check_output(["rustc", "--print", "host-tuple"], cwd=root / "desktop", text=True).strip()
supplemental = json.loads((root / "licenses/manifest.json").read_text())

def extra_notices(records):
    result = []
    for record in records:
        path = root / "licenses" / record["path"]
        if path.parent != root / "licenses":
            raise SystemExit("Supplemental license path must be a direct child of licenses/")
        data = path.read_bytes()
        if hashlib.sha256(data).hexdigest() != record["sha256"]:
            raise SystemExit(f"Supplemental license checksum mismatch: {path.name}")
        result.append(f"\n--- {record.get('component', path.name)} ---\nSource: {record['source']}\n")
        result.append(data.decode("utf-8") + "\n")
    return result
metadata = json.loads(subprocess.check_output([
    cargo, "metadata", "--locked", "--format-version", "1", "--filter-platform", target
], cwd=root / "desktop", text=True))
active = {node["id"] for node in metadata["resolve"]["nodes"]}
parts = ["Relay desktop: dependency licenses and source manifest\n", f"Target: {target}\n",
         "System webview libraries are supplied by the operating system or its WebView2 runtime installer.\n",
         "MPL-2.0-covered dependency source is available under MPL-2.0 at the exact source archive URLs below.\n",
         "Dependencies are unmodified. Their licenses and source rights are not replaced by Relay's Apache-2.0 license.\n"]
for package in sorted(metadata["packages"], key=lambda p: (p["name"], p["version"])):
    if not package["source"] or package["id"] not in active:
        continue
    directory = Path(package["manifest_path"]).parent
    parts.append(f"\n{'=' * 72}\n{package['name']} {package['version']}\n")
    parts.append(f"Declared license: {package.get('license') or 'See license file'}\n")
    parts.append(f"Exact upstream source: https://crates.io/api/v1/crates/{package['name']}/{package['version']}/download\n")
    if package.get("repository"):
        parts.append(f"Repository: {package['repository']}\n")
    files = [p for p in directory.iterdir() if p.is_file() and p.name.upper().startswith(("LICENSE", "LICENCE", "COPYING", "NOTICE"))]
    if package.get("license_file"):
        specified = directory / package["license_file"]
        if specified.is_file():
            files.append(specified)
    if not files:
        key = f"{package['name']}@{package['version']}"
        records = supplemental["crates"].get(key)
        if not records:
            raise SystemExit(f"No complete license text for {key}; review before packaging")
        parts.extend(extra_notices(records))
    for path in sorted(set(files)):
        parts.append(f"\n--- {path.name} ---\n{path.read_text(errors='replace')}\n")
    key = f"{package['name']}@{package['version']}"
    parts.extend(extra_notices(supplemental["extra_by_crate"].get(key, [])))

# Cargo metadata excludes the statically linked Rust standard library. Its
# toolchain-supplied copyright report includes its own dependency notices.
class ReadableHTML(HTMLParser):
    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.parts = []
        self.hidden = 0

    def handle_starttag(self, tag, attrs):
        if tag in {"head", "style", "script"}:
            self.hidden += 1
        if not self.hidden and tag in {"p", "div", "li", "h1", "h2", "h3", "pre", "summary", "br"}:
            self.parts.append("\n")

    def handle_endtag(self, tag):
        if tag in {"head", "style", "script"}:
            self.hidden -= 1
        if not self.hidden and tag in {"p", "div", "li", "h1", "h2", "h3", "pre", "summary"}:
            self.parts.append("\n")

    def handle_data(self, data):
        if not self.hidden:
            self.parts.append(data)

sysroot = Path(subprocess.check_output(["rustc", "--print", "sysroot"], cwd=root / "desktop", text=True).strip())
copyright_file = sysroot / "share/doc/rust/COPYRIGHT-library.html"
if not copyright_file.is_file():
    raise SystemExit("Rust standard-library copyright report is missing; install the rust-docs toolchain component before packaging")
reader = ReadableHTML()
reader.feed(copyright_file.read_text(encoding="utf-8"))
parts.append("\n" + "=" * 72 + "\nRust standard library and toolchain-supplied dependency notices\n")
parts.append(subprocess.check_output(["rustc", "--version", "--verbose"], cwd=root / "desktop", text=True))
parts.append("".join(reader.parts))
license_dir = sysroot / "share/doc/rust/licenses"
for path in sorted(license_dir.glob("*.txt")):
    parts.append(f"\n--- Rust toolchain license text: {path.name} ---\n{path.read_text(encoding='utf-8')}\n")
destination = args.output or root / "desktop/resources/RUST_THIRD_PARTY_NOTICES.txt"
destination.parent.mkdir(parents=True, exist_ok=True)
destination.write_text("".join(parts), encoding="utf-8")
print(f"Wrote desktop license/source manifest for {len(active)} resolved packages.")
