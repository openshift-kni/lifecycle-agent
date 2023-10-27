#!/bin/bash
# shellcheck disable=SC2155
set -e
set -o pipefail

HACKDIR=$( dirname $( readlink -f "${BASH_SOURCE[0]}" ) )
BASEDIR="${HACKDIR}/.."

# Define version string
export GO_PACKAGE=$(go list -mod=vendor -m -f '{{ .Path }}')
export SOURCE_GIT_BRANCH=$(git branch --show-current 2>/dev/null)
export SOURCE_GIT_COMMIT=$(git rev-parse --short "HEAD^{commit}" 2>/dev/null)
# The formal build modifies the Dockerfile, so restrict the git-tree check to specific dirs/files
export SOURCE_GIT_TREE_STATE=$(git diff --quiet cmd pkg vendor go.mod go.sum main.go >&/dev/null || echo '+dirty')
export BUILD_DATE=$(date -u +'%Y%m%d.%H%M%S')
export VERSION_STR="${BUILD_DATE}+${SOURCE_GIT_BRANCH}.${SOURCE_GIT_COMMIT}${SOURCE_GIT_TREE_STATE}"

export GO111MODULE=on
export GOPROXY=off
export GOFLAGS=-mod=vendor
export GOOS=linux
export GOARCH=amd64
export CGO_ENABLED=0

go build \
    -ldflags="-X '${GO_PACKAGE}/cmd.Version=${VERSION_STR}'" \
    -v -o "${BASEDIR}/_output/factory-precaching-cli" main.go
