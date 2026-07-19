#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_DIR="$ROOT_DIR/iosApp/Frameworks"
OUT_FRAMEWORK="$OUT_DIR/GoTorrent.xcframework"

if ! command -v gomobile >/dev/null 2>&1; then
  echo "gomobile is not installed. Installing golang.org/x/mobile/cmd/gomobile@latest..."
  go install golang.org/x/mobile/cmd/gomobile@latest
fi

mkdir -p "$OUT_DIR"
rm -rf "$OUT_FRAMEWORK"

pushd "$ROOT_DIR/go-torrent" >/dev/null
gomobile init
gomobile bind -target=ios -iosversion=16.1 -o "$OUT_FRAMEWORK" .
popd >/dev/null

echo "Built $OUT_FRAMEWORK"
