#!/bin/bash

set -o errexit
set -o nounset
set -o pipefail

export CGO_ENABLED=1 # the race detector needs cgo

echo "Running tests:"
go test -race -cover -count=1 ./...
echo
