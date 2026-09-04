#!/bin/bash
# ProgramBench-style build: must produce ./executable at the repo root.
# gowsdl's -v flag prints main.Version, injected at build time (empty otherwise,
# so `gowsdl -v` prints just the emoji prefix). The gold binary embedded v0.5.0.
set -e
cd "$(dirname "$0")"
go build -buildvcs=false -ldflags="-X main.Version=v0.5.0" -o executable ./cmd/gowsdl
