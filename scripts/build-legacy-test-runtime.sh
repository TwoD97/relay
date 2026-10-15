#!/usr/bin/env bash
# Build the actual pre-Mastermind public runtime in an isolated checkout. The
# digest must match the originally shipped binary before any test executes it.
set -euo pipefail
cd "$(dirname "$0")/.."
RELAY_LEGACY_REF=fd7999b3bbcf20c8d852bf7461a58e211d04a376
RELAY_LEGACY_VERSION=0.1.0-dev.50e9fc609217
RELAY_LEGACY_SHA256=f3483628e68655cfae23beaf8f0cfd752ee11035708c8564563d1bf85bbbf31f
RELAY_GO="${RELAY_GO:-go}"
if [[ $# -gt 1 ]]; then echo 'Usage: build-legacy-test-runtime.sh [output-file]' >&2; exit 1; fi
if [[ $("$RELAY_GO" env GOVERSION) != go1.27.1 ]]; then
  echo 'The immutable legacy runtime fixture requires Go 1.27.1.' >&2
  exit 1
fi
if [[ $(uname -s) != Linux || $(uname -m) != x86_64 ]]; then
  echo 'The legacy runtime fixture is for Linux amd64 integration tests.' >&2
  exit 1
fi
if ! git cat-file -e "$RELAY_LEGACY_REF^{commit}" 2>/dev/null; then
  printf 'Fetch the fixed public fixture first: git fetch --no-tags --depth=1 origin %s\n' "$RELAY_LEGACY_REF" >&2
  exit 1
fi
RELAY_LEGACY_OUTPUT=$(realpath -m -- "${1:-dist/relay-legacy-linux-amd64}")
RELAY_LEGACY_BUILD=$(mktemp -d "${TMPDIR:-/tmp}/relay-legacy-runtime.XXXXXX")
trap 'rm -rf -- "$RELAY_LEGACY_BUILD"' EXIT
git archive "$RELAY_LEGACY_REF" | tar -x -C "$RELAY_LEGACY_BUILD"
(
  cd "$RELAY_LEGACY_BUILD"
  npm --prefix web ci --no-audit --no-fund
  npm --prefix web run build
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 "$RELAY_GO" build \
    -trimpath -buildvcs=false -ldflags "-s -w -X main.version=$RELAY_LEGACY_VERSION" \
    -o relay-legacy-linux-amd64 ./cmd/relay
  printf '%s  relay-legacy-linux-amd64\n' "$RELAY_LEGACY_SHA256" | sha256sum -c -
  [[ $(./relay-legacy-linux-amd64 --version) == "$RELAY_LEGACY_VERSION" ]]
)
mkdir -p -- "$(dirname "$RELAY_LEGACY_OUTPUT")"
install -m 700 -- "$RELAY_LEGACY_BUILD/relay-legacy-linux-amd64" "$RELAY_LEGACY_OUTPUT"
printf 'Verified legacy fixture %s (%s).\n' "$RELAY_LEGACY_VERSION" "$RELAY_LEGACY_SHA256"
