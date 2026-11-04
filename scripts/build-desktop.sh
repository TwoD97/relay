#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
if ! command -v cargo >/dev/null && [ -x "$HOME/.cargo/bin/cargo" ]; then
  export PATH="$HOME/.cargo/bin:$PATH"
fi
case "$(uname -sm)" in
  'Linux x86_64') RELAY_DESKTOP_ARCH=amd64 ;;
  'Linux aarch64'|'Linux arm64') RELAY_DESKTOP_ARCH=arm64 ;;
  *) echo 'The desktop build currently supports Linux and WSLg.' >&2; exit 1 ;;
esac
command -v cargo >/dev/null || { echo 'Install Rust using rustup before building the desktop client.' >&2; exit 1; }
pkg-config --exists webkit2gtk-4.1 gtk+-3.0 || {
  echo 'Install the Linux Tauri prerequisites documented in desktop/README.md.' >&2; exit 1;
}
./scripts/build.sh
python3 - <<'PY'
from pathlib import Path
import hashlib, shutil
source = Path('dist')
target = Path('desktop/resources/runtime')
target.mkdir(parents=True, exist_ok=True)
for line in (source/'SHA256SUMS').read_text().splitlines():
    digest, name = line.split()
    if name not in ('relay-linux-amd64', 'relay-linux-arm64'):
        raise SystemExit('Unexpected runtime artifact in checksum manifest')
    if hashlib.sha256((source/name).read_bytes()).hexdigest() != digest:
        raise SystemExit('Runtime checksum mismatch')
    shutil.copy2(source/name, target/name)
    (target/name).chmod(0o755)
for name in ('SHA256SUMS', 'VERSION', 'LICENSE', 'NOTICE', 'THIRD_PARTY_NOTICES.md', 'THIRD_PARTY_NOTICES.txt', 'README.md'):
    shutil.copy2(source/name, target/name)
optional = ('relay-whatsapp-personal-linux-amd64', 'relay-whatsapp-personal-linux-arm64', 'relay-whatsapp-personal-windows-amd64.exe')
checksums = {}
for line in (source/'SHA256SUMS.whatsapp').read_text().splitlines():
    digest, name = line.split()
    if name not in optional or name in checksums:
        raise SystemExit('Unexpected optional connector manifest entry')
    checksums[name] = digest
if set(checksums) != set(optional):
    raise SystemExit('Optional connector manifest is incomplete')
for name, digest in checksums.items():
    if hashlib.sha256((source/name).read_bytes()).hexdigest() != digest:
        raise SystemExit('Optional connector checksum mismatch')
    shutil.copy2(source/name, target/name)
    (target/name).chmod(0o755)
for name in ('SHA256SUMS.whatsapp', 'WHATSAPP_PERSONAL_LICENSE.txt', 'WHATSAPP_PERSONAL_NOTICES.txt', 'relay-whatsapp-personal-source.tar.gz'):
    shutil.copy2(source/name, target/name)
shutil.copytree(source/'docs', target/'docs', dirs_exist_ok=True)
PY
npm --prefix desktop ci --no-audit --no-fund
python3 scripts/desktop_notices.py
(cd desktop && npm run build -- -- --locked)
python3 - "$RELAY_DESKTOP_ARCH" <<'PY'
from pathlib import Path
import os, shutil, sys
target_root = Path(os.environ.get("CARGO_TARGET_DIR", "target"))
if not target_root.is_absolute():
    target_root = Path("desktop") / target_root
packages = sorted((target_root/"release/bundle/deb").glob("*.deb"), key=lambda p: p.stat().st_mtime)
if not packages:
    raise SystemExit('The desktop build did not produce a Debian package')
target = Path('dist') / f'relay-desktop-linux-{sys.argv[1]}.deb'
shutil.copy2(packages[-1], target)
print(f'Built {target}. Install with: sudo apt install "{target.resolve()}"')
PY
