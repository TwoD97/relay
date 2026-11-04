#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"
exec python3 "$root/scripts/build-whatsapp-personal.py" "${1:-dist}"
