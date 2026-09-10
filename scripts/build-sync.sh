#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p dist
cd sync
for os in windows darwin; do
  for arch in amd64 arm64; do
    suffix=''; if [ "$os" = windows ]; then suffix='.exe'; fi
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags='-s -w' -o "../dist/coppy-$os-$arch$suffix" .
  done
done
