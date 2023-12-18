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
ROLLBACK_PROG=$(getCondition RollbackInProgress status)
ROLLBACK_COMPLETE=$(getCondition RollbackCompleted status)

echo "Image:   ${SEED}"
echo "Version: ${VER}"
echo "Stage:   ${STAGE}"
echo "Status:"

if [ "${ROLLBACK_COMPLETE}" == "True" ]; then
    echo "  Rollback completed at: $(getCondition RollbackCompleted lastTransitionTime)"
elif [ "${ROLLBACK_COMPLETE}" == "False" ] && [ "${ROLLBACK_PROG}" == "False" ]; then
    echo "  Rollback failed:"
    echo "    Time:    $(getCondition RollbackInProgress lastTransitionTime)"
    echo "    Reason:  $(getCondition RollbackInProgress reason)"
    echo "    Message: $(getCondition RollbackInProgress message)"
elif [ "${ROLLBACK_PROG}" == "True" ]; then
    echo "  Rollback in progress at: $(getCondition RollbackInProgress lastTransitionTime)"
elif [ "${UPG_COMPLETE}" == "True" ]; then
    echo "  Upgrade completed at: $(getCondition UpgradeCompleted lastTransitionTime)"
elif [ "${UPG_COMPLETE}" == "False" ] && [ "${UPG_PROG}" == "False" ]; then
    echo "  Upgrade failed:"
    echo "    Time:    $(getCondition UpgradeInProgress lastTransitionTime)"
    echo "    Reason:  $(getCondition UpgradeInProgress reason)"
    echo "    Message: $(getCondition UpgradeInProgress message)"
elif [ "${UPG_PROG}" == "True" ]; then
    echo "  Upgrade in progress at: $(getCondition UpgradeInProgress lastTransitionTime)"
elif [ "${PREP_COMPLETE}" == "True" ]; then
    echo "  Prep completed at: $(getCondition PrepCompleted lastTransitionTime)"
elif [ "${PREP_COMPLETE}" == "False" ] && [ "${PREP_PROG}" == "False" ]; then
    echo "  Prep failed:"
    echo "    Time:    $(getCondition PrepInProgress lastTransitionTime)"
    echo "    Reason:  $(getCondition PrepInProgress reason)"
    echo "    Message: $(getCondition PrepInProgress message)"
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
