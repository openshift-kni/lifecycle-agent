#!/usr/bin/env bash

# Set recert image to environment
if [[ -f "/recert_image.sh" ]]; then
    # This script will only exist inside the dockerfile, so shellcheck cannot verify it exists
    # shellcheck disable=SC1091
    source /recert_image.sh
    echo "Set RELATED_IMAGE_RECERT_IMAGE to '${RELATED_IMAGE_RECERT_IMAGE}'"
else
    echo "Could not find /tmp/recert_image.sh"
fi

# Start the manager binary
/usr/local/bin/manager
