#!/bin/bash

set -eou pipefail

EXCLUDES="$@"
: ${EXCLUDES:=vendor}
COMPANY_PREFIXES="github.com/masudur-rahman"
IMPORTS_ORDER="std,project,company,general,blanked"

format_go_files() {
    echo "Running goimports-reviser:"
    cmd="goimports-reviser -recursive -company-prefixes=${COMPANY_PREFIXES} -imports-order=${IMPORTS_ORDER} -format -excludes ${EXCLUDES} ./..."
    echo "$cmd"
    $cmd
}

format_script_files() {
    echo "Running shfmt:"
    cmd="find . -path ./vendor -prune -o -name '*.sh' -exec shfmt -l -w -ci -i 4 {} \;"
    echo "$cmd"
    eval "$cmd"
    echo
}

format_go_files
format_script_files
