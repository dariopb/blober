#!/usr/bin/env sh
set -eu

CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o blober ./cmd/blober
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o blober.exe ./cmd/blober
