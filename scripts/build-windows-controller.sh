#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
RELAY_GO="${RELAY_GO:-go}"
if [ ! -f dist/VERSION ]; then
  echo 'Run scripts/build.sh first to prepare the matching Linux payloads.' >&2
  exit 1
fi
RELAY_WINDOWS_VERSION=$(cat dist/VERSION)
if [[ ! "$RELAY_WINDOWS_VERSION" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,100}$ ]]; then
  echo 'Invalid runtime bundle version.' >&2
  exit 1
fi
(cd dist && sha256sum --check --strict SHA256SUMS)
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 "$RELAY_GO" build -trimpath -buildvcs=false \
  -ldflags "-s -w -X main.version=$RELAY_WINDOWS_VERSION" \
  -o dist/relay-controller-windows-amd64.exe ./cmd/relay
(cd dist && sha256sum relay-controller-windows-amd64.exe > SHA256SUMS.windows)
printf 'Built the native Windows controller %s.\n' "$RELAY_WINDOWS_VERSION"
