#!/bin/bash
#
# Helper script for monitoring IBU CR during an upgrade
# Parses the IBU CR to display a brief summary of key information
#

json=$(oc get ibu upgrade -o json 2>/dev/null)

if [ -z "${json}" ]; then
    echo "Unable to get IBU CR" >&2
    exit 1
fi

function getCondition {
    local ctype="$1"
    local field="$2"
    echo "${json}" | jq -r --arg ctype "${ctype}" --arg field "${field}" '.status.conditions[] | select(.type==$ctype)[$field]'
}

SEED=$(echo "${json}" | jq -r '.spec.seedImageRef.image')
VER=$(echo "${json}" | jq -r '.spec.seedImageRef.version')
STAGE=$(echo "${json}" | jq -r '.spec.stage')

IDLE=$(getCondition Idle status)
PREP_PROG=$(getCondition PrepInProgress status)
PREP_COMPLETE=$(getCondition PrepCompleted status)
UPG_PROG=$(getCondition UpgradeInProgress status)
UPG_COMPLETE=$(getCondition UpgradeCompleted status)

echo "Image:   ${SEED}"
echo "Version: ${VER}"
echo "Stage:   ${STAGE}"
echo "Status:"

if [ "${UPG_COMPLETE}" == "True" ]; then
    echo "  Upgrade completed at: $(getCondition UpgradeCompleted lastTransitionTime)"
elif [ "${UPG_COMPLETE}" == "False" ] && [ "${UPG_PROG}" == "False" ]; then
    echo "  Upgrade failed:"
    echo "    Time:    $(getCondition UpgradeCompleted lastTransitionTime)"
    echo "    Reason:  $(getCondition UpgradeCompleted reason)"
    echo "    Message: $(getCondition UpgradeCompleted message)"
elif [ "${UPG_PROG}" == "True" ]; then
    echo "  Upgrade in progress at: $(getCondition UpgradeInProgress lastTransitionTime)"
elif [ "${PREP_COMPLETE}" == "True" ]; then
    echo "  Prep completed at: $(getCondition PrepCompleted lastTransitionTime)"
elif [ "${PREP_COMPLETE}" == "False" ] && [ "${PREP_PROG}" == "False" ]; then
    echo "  Prep failed:"
    echo "    Time:    $(getCondition PrepCompleted lastTransitionTime)"
    echo "    Reason:  $(getCondition PrepCompleted reason)"
    echo "    Message: $(getCondition PrepCompleted message)"
elif [ "${PREP_PROG}" == "True" ]; then
    echo "  Prep in progress at: $(getCondition PrepInProgress lastTransitionTime)"
elif [ "${IDLE}" == "True" ]; then
    echo "  Idle. Last transition time: $(getCondition Idle lastTransitionTime)"
elif [ "${IDLE}" == "False" ]; then
    echo "  Idle False!!!"
    echo "    Time:    $(getCondition Idle lastTransitionTime)"
    echo "    Reason:  $(getCondition Idle reason)"
    echo "    Message: $(getCondition Idle message)"
else
    echo "  Unhandled state!!!"
fi
