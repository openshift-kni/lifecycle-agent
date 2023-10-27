#!/bin/bash

export GOCACHE=/tmp/
export GOLANGCI_LINT_CACHE=/tmp/.cache


if ! which golangci-lint &> /dev/null; then
    echo "Downloading golangci-lint tool..."
    if ! curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -d -b "$(go env GOPATH)/bin" v1.51.1; then
        echo "Install from script failed. Trying 'go install'..."
        if ! go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.51.1; then
            echo "Install of golangci-lint failed"
            exit 1
        fi
    fi
fi

golangci-lint run --verbose --print-resources-usage --modules-download-mode=vendor --timeout=5m0s
