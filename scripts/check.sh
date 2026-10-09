#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
RELAY_GO="${RELAY_GO:-go}"
npm --prefix web ci --no-audit --no-fund
npm --prefix web run build
GOFMT="$(dirname "$(command -v "$RELAY_GO")")/gofmt"
unformatted="$(find cmd internal skills web -type f -name '*.go' -print0 | xargs -0 "$GOFMT" -l)"
if [ -n "$unformatted" ]; then printf 'Go formatting required:\n%s\n' "$unformatted" >&2; exit 1; fi
"$RELAY_GO" vet ./...
"$RELAY_GO" test -race -count=1 ./...
python3 scripts/test_install.py -v
npm --prefix web test
