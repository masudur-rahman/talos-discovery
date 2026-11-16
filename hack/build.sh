#!/bin/bash

set -o errexit
set -o nounset
set -o pipefail

if [ -z "${VERSION:-}" ]; then
    VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
fi

export CGO_ENABLED=0
export GOOS="${OS:-$(go env GOOS)}"
export GOARCH="${ARCH:-$(go env GOARCH)}"

if [[ "${DEBUG:-}" == 1 ]]; then
    # Debugging - disable optimizations and inlining
    gogcflags="all=-N -l"
    goldflags=""
else
    gogcflags=""
    goldflags="-s -w"
fi

go build \
    -trimpath \
    -gcflags="${gogcflags}" \
    -ldflags="-X main.version=${VERSION} ${goldflags}" \
    -o "bin/talos-discovery-${GOOS}-${GOARCH}" \
    .
