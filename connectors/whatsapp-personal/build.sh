#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-only
# Build from the vendored corresponding-source archive, without a module proxy.
set -eu
cd "$(dirname "$0")"
GO=${GO:-go}
case "$("$GO" version)" in *'go1.27.1 '*) ;; *) echo 'Use Go 1.27.1 for this release.' >&2; exit 1;; esac
mkdir -p out
for target in linux-amd64 linux-arm64 windows-amd64; do
  system=${target%-*}
  architecture=${target#*-}
  suffix=
  if [ "$system" = windows ]; then suffix=.exe; fi
  CGO_ENABLED=0 GOOS="$system" GOARCH="$architecture" GOPROXY=off GOSUMDB=off GOWORK=off GOTOOLCHAIN=local "$GO" build -mod=vendor -trimpath -buildvcs=false -ldflags='-s -w' -o "out/relay-whatsapp-personal-$target$suffix" .
done
