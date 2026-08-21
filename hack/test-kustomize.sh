#!/bin/bash

set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Temporary file tracking for cleanup
TEMP_FILES=()

# Cleanup function (invoked via trap, not called directly)
# shellcheck disable=SC2329
cleanup() {
    # macOS Bash treats "${array[@]}" as unbound under set -u when empty
    if ((${#TEMP_FILES[@]} > 0)); then
        for temp_file in "${TEMP_FILES[@]}"; do
            if [ -f "$temp_file" ]; then
                rm -f "$temp_file"
                echo "Cleaned up: $temp_file"
            fi
        done
    fi
}

# Set trap to ensure cleanup on exit
trap cleanup EXIT

# Check if kustomize is installed
if ! command -v kustomize &> /dev/null; then
    echo -e "${RED}ERROR: kustomize is not installed${NC}"
    echo "Install via: make kustomize"
    exit 1
fi

# Generate temporary patch files that are normally created during build.
# config/manager/related-images/patch.yaml is generated from in.yaml with envsubst.
PATCH_FILE="./config/manager/related-images/patch.yaml"
if [ ! -f "$PATCH_FILE" ] && [ -f "./config/manager/related-images/in.yaml" ]; then
    if ! command -v envsubst &> /dev/null; then
        echo -e "${RED}ERROR: envsubst is not installed (provided by gettext)${NC}"
        exit 1
    fi
    echo "Generating temporary patch file for validation: $PATCH_FILE"
    PRECACHE_WORKLOAD_IMG="quay.io/openshift-kni/lifecycle-agent-operator:latest" \
        envsubst < "./config/manager/related-images/in.yaml" > "$PATCH_FILE"
    TEMP_FILES+=("$PATCH_FILE")
fi

echo "Checking all kustomization.yaml files can build successfully..."
echo ""

ERRORS=0
CHECKED=0

# Find all kustomization.yaml files
kustomize_files=()
while IFS= read -r file; do
    kustomize_files+=("$file")
done < <(find . -name 'kustomization.yaml' -not -path '*/vendor/*' -not -path '*/.git/*' -not -path '*/bin/*' -not -path '*/telco5g-konflux/*' | sort)

if [ ${#kustomize_files[@]} -eq 0 ]; then
    echo -e "${YELLOW}WARNING: No kustomization.yaml files found${NC}"
    exit 0
fi

for kustomize_file in "${kustomize_files[@]}"; do
    dir=$(dirname "$kustomize_file")
    echo -n "  $dir: "

    if BUILD_OUTPUT=$(kustomize build "$dir" 2>&1); then
        echo -e "${GREEN}OK${NC}"
    else
        echo -e "${RED}FAILED${NC}"
        echo -e "${YELLOW}    Error details:${NC}"
        echo "    ${BUILD_OUTPUT//$'\n'/$'\n'    }"
        echo ""
        ERRORS=$((ERRORS + 1))
    fi
    CHECKED=$((CHECKED + 1))
done

echo ""
echo "Summary: Checked $CHECKED kustomization.yaml file(s)"

if [[ $ERRORS -eq 0 ]]; then
    echo -e "${GREEN}All kustomization files validated successfully!${NC}"
    exit 0
else
    echo -e "${RED}$ERRORS kustomization file(s) failed validation${NC}"
    exit 1
fi
