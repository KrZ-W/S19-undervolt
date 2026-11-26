#!/bin/sh
# Build the ARM binaries the installer ships (run from anywhere with Go installed).
set -e
here=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$here/bin"
export GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0
(cd "$here/.." && go build -trimpath -ldflags="-s -w" -o "$here/bin/s19volt" ./cmd/s19volt \
                && go build -trimpath -ldflags="-s -w" -o "$here/bin/ch3test" ./cmd/ch3test)
(cd "$here/../s19regs" && go build -trimpath -ldflags="-s -w" -o "$here/bin/s19regs" .)
ls -l "$here/bin"
