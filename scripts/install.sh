#!/usr/bin/env bash
# Install a complete local client bundle. Remote hosts install from that bundle over SSH.
set -euo pipefail
RELAY_BUNDLE=$(cd "$(dirname "$0")" && pwd)
RELAY_CALLER_DIR="$PWD"
case "$(uname -sm)" in
  'Linux x86_64') RELAY_ARCH=amd64 ;;
  'Linux aarch64'|'Linux arm64') RELAY_ARCH=arm64 ;;
  *) echo 'Relay supports Linux and WSL on amd64/arm64.' >&2; exit 1 ;;
esac
cd "$RELAY_BUNDLE"
verify_bundle() {
  awk 'NF != 2 || length($1) != 64 || $1 !~ /^[a-fA-F0-9]+$/ || ($2 != "relay-linux-amd64" && $2 != "relay-linux-arm64") { bad=1 }
    { seen[$2]++ }
    END { exit (bad || seen["relay-linux-amd64"] != 1 || seen["relay-linux-arm64"] != 1) }' SHA256SUMS || { echo 'Checksum manifest must contain exactly one checksum for each runtime architecture.' >&2; return 1; }
  sha256sum --check --strict SHA256SUMS
}
verify_bundle
RELAY_VERSION=$(cat VERSION)
if [[ ! "$RELAY_VERSION" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,100}$ ]]; then echo 'Invalid bundle version' >&2; exit 1; fi
RELAY_INSTALL_ROOT="${RELAY_INSTALL_ROOT:-$HOME/.local/share/relay-client}"
RELAY_INSTALL_BIN="${RELAY_INSTALL_BIN:-$HOME/.local/bin}"
case "$RELAY_INSTALL_ROOT" in /*) ;; *) RELAY_INSTALL_ROOT="$RELAY_CALLER_DIR/$RELAY_INSTALL_ROOT" ;; esac
case "$RELAY_INSTALL_BIN" in /*) ;; *) RELAY_INSTALL_BIN="$RELAY_CALLER_DIR/$RELAY_INSTALL_BIN" ;; esac
umask 077
mkdir -p "$RELAY_INSTALL_ROOT/releases" "$RELAY_INSTALL_BIN"
RELAY_INSTALL_ROOT=$(cd "$RELAY_INSTALL_ROOT" && pwd -P)
RELAY_INSTALL_BIN=$(cd "$RELAY_INSTALL_BIN" && pwd -P)
RELAY_RELEASE="$RELAY_INSTALL_ROOT/releases/$RELAY_VERSION"
if [ -e "$RELAY_RELEASE" ] || [ -L "$RELAY_RELEASE" ]; then
  [ -d "$RELAY_RELEASE" ] && [ ! -L "$RELAY_RELEASE" ] || { echo 'Release destination is not a regular directory.' >&2; exit 1; }
  cmp "$RELAY_RELEASE/SHA256SUMS" "$RELAY_BUNDLE/SHA256SUMS" || { echo 'A different release already uses this version.' >&2; exit 1; }
  (cd "$RELAY_RELEASE" && verify_bundle)
else
  RELAY_STAGING=$(mktemp -d "$RELAY_INSTALL_ROOT/releases/.install.XXXXXXXX")
  trap 'rm -rf "$RELAY_STAGING"' EXIT
  cp relay-linux-amd64 relay-linux-arm64 SHA256SUMS VERSION "$RELAY_STAGING/"
  for RELAY_NOTICE in README.md LICENSE NOTICE THIRD_PARTY_NOTICES.md THIRD_PARTY_NOTICES.txt; do
    if [ -f "$RELAY_NOTICE" ]; then cp "$RELAY_NOTICE" "$RELAY_STAGING/"; fi
  done
  if [ -d docs ]; then cp -R docs "$RELAY_STAGING/"; fi
  chmod 700 "$RELAY_STAGING"/relay-linux-*
  (cd "$RELAY_STAGING" && verify_bundle && [ "$(cat VERSION)" = "$RELAY_VERSION" ])
  if ! mv -T --no-clobber "$RELAY_STAGING" "$RELAY_RELEASE"; then
    [ -d "$RELAY_RELEASE" ] && [ ! -L "$RELAY_RELEASE" ] || exit 1
  fi
  # A concurrent installer may have won the atomic activation.
  [ -d "$RELAY_RELEASE" ] && [ ! -L "$RELAY_RELEASE" ] || { echo 'Release destination changed.' >&2; exit 1; }
  cmp "$RELAY_RELEASE/SHA256SUMS" "$RELAY_BUNDLE/SHA256SUMS"
  (cd "$RELAY_RELEASE" && verify_bundle)
  rm -rf "$RELAY_STAGING"
  trap - EXIT
fi
RELAY_LINK_DIR=$(mktemp -d "$RELAY_INSTALL_BIN/.relay-link.XXXXXXXX")
trap 'rm -rf "$RELAY_LINK_DIR"' EXIT
if [ -d "$RELAY_INSTALL_BIN/relay" ] && [ ! -L "$RELAY_INSTALL_BIN/relay" ]; then
  echo "Command destination is a directory: relay" >&2; exit 1
fi
ln -s "$RELAY_RELEASE/relay-linux-$RELAY_ARCH" "$RELAY_LINK_DIR/relay"
mv -fT "$RELAY_LINK_DIR/relay" "$RELAY_INSTALL_BIN/relay"
printf 'Installed Relay %s. Start with:\n  %s/relay ui\n' "$RELAY_VERSION" "$RELAY_INSTALL_BIN"
