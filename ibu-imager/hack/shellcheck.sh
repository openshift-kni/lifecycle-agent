#!/bin/bash

# Set the ShellCheck version and binary path
shellcheck_version="v0.7.2"
shellcheck_binary="./bin/shellcheck"


function cleanup {
    # Clean up the temporary directory
    [ -n "$temp_dir" ] && rm -rf "$temp_dir"
}
trap cleanup EXIT

function fatal {
    # Log an error message and exit with a non-zero status
    echo "ERROR: $*" >&2
    exit 1
}

# Check if ShellCheck binary is not found
if [ ! -f "${shellcheck_binary}" ]; then
    echo "Downloading ShellCheck tool..."
    download_url="https://github.com/koalaman/shellcheck/releases/download/$shellcheck_version/shellcheck-$shellcheck_version.linux.x86_64.tar.xz"

    # Create a temporary directory for extraction
    temp_dir="$(mktemp -d)" || fatal "Failed to create a temporary directory"

    # Download and extract ShellCheck
    wget -qO- "$download_url" | tar -xJ -C "$temp_dir" --strip=1 "shellcheck-$shellcheck_version/shellcheck" || fatal "Failed to download and extract ShellCheck"

    # Move the ShellCheck binary to the desired location
    mv "$temp_dir/shellcheck" "$shellcheck_binary" || fatal "Failed to move ShellCheck binary"

    echo "ShellCheck has been installed"
fi

# Find and check shell script files with ShellCheck
find . -name '*.sh' -not -path './vendor/*' -not -path './git/*' \
    -not -path './bin/*' -not -path './testbin/*' -print0 |
    xargs -0 --no-run-if-empty "${shellcheck_binary}" -x || fatal "ShellCheck encountered errors"

# All checks passed successfully
echo "All checks passed successfully"
exit 0
