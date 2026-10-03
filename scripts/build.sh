#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
mkdir -p bin
version="${VERSION:-dev}"
for target in linux/amd64 linux/arm64 linux/arm darwin/amd64 darwin/arm64 windows/amd64 windows/arm64 freebsd/amd64; do
  probe_os="${target%/*}"
  probe_arch="${target#*/}"
  suffix=""
  [ "$probe_os" != windows ] || suffix=.exe
  for flavor in standard nootel; do
    tags=""
    [ "$flavor" != nootel ] || tags=nootel
    CGO_ENABLED=0 GOOS="$probe_os" GOARCH="$probe_arch" GOARM=7 go build -tags "$tags" -trimpath \
      -ldflags="-s -w -X main.version=$version" \
      -o "bin/light-prober-$probe_os-$probe_arch-$flavor$suffix" ./cmd/light-prober
  done
done
