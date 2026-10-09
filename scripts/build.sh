#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
RELAY_GO="${RELAY_GO:-go}"
command -v "$RELAY_GO" >/dev/null || { echo 'Go 1.26+ is required (or set RELAY_GO to the Go executable).' >&2; exit 1; }
command -v npm >/dev/null || { echo 'Node.js 22+ and npm are required to build the browser UI.' >&2; exit 1; }
npm --prefix web ci --no-audit --no-fund
npm --prefix web run build
RELAY_SOURCE_HASH=$(python3 - <<'PY'
import hashlib,pathlib
root=pathlib.Path('.')
files=[]
for pattern in ('cmd/**/*.go','internal/**/*.go','internal/**/*.asc','skills/**/*','web/src/**/*','web/dist/**/*','web/embed.go','go.mod','go.sum','web/package.json','web/package-lock.json'):
    files.extend(p for p in root.glob(pattern) if p.is_file())
h=hashlib.sha256()
for p in sorted(set(files)):
    h.update(str(p).encode()+b'\0'+p.read_bytes()+b'\0')
print(h.hexdigest()[:12])
PY
)
RELAY_VERSION="${RELAY_VERSION:-0.1.0-dev.$RELAY_SOURCE_HASH}"
if [[ ! "$RELAY_VERSION" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,100}$ ]]; then echo 'Invalid RELAY_VERSION' >&2; exit 1; fi
mkdir -p dist
for RELAY_ARCH in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$RELAY_ARCH" "$RELAY_GO" build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=$RELAY_VERSION" -o "dist/relay-linux-$RELAY_ARCH" ./cmd/relay
done
(
  cd dist
  sha256sum relay-linux-amd64 relay-linux-arm64 > SHA256SUMS
)
cp scripts/install.sh dist/install.sh
cp README.md dist/README.md
cp LICENSE NOTICE THIRD_PARTY_NOTICES.md dist/
mkdir -p dist/docs
cp docs/CONTRACT.md docs/VALIDATION.md docs/SHARED_CONTEXT.md dist/docs/
RELAY_GO="$RELAY_GO" python3 scripts/notices.py
printf '%s\n' "$RELAY_VERSION" > dist/VERSION
tar -C dist --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner -czf dist/relay-linux-bundle.tar.gz relay-linux-amd64 relay-linux-arm64 SHA256SUMS VERSION install.sh README.md LICENSE NOTICE THIRD_PARTY_NOTICES.md THIRD_PARTY_NOTICES.txt docs
printf 'Built Relay %s. Run dist/install.sh, then ~/.local/bin/relay ui.\n' "$RELAY_VERSION"
