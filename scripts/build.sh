#!/bin/bash
# Builds ws and ws-server into <repo>/bin/.
set -euo pipefail

cd "$(dirname "$0")/.."
mkdir -p bin

# Everything is pure Go (modernc.org/sqlite needs no cgo), and
# disabling cgo keeps the build independent of the host C compiler.
export CGO_ENABLED=0
go build -o bin/ws ./cmd/ws
go build -o bin/ws-server ./cmd/ws-server
echo "Built bin/ws and bin/ws-server"
