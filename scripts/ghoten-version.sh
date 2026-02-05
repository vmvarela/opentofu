#!/usr/bin/env bash
# Ghoten Version Calculator
# Generates the next version in AA.m.d format based on existing tags.
#
# Format: vAA.m.d
#   AA = Last two digits of the year (e.g., 26 for 2026)
#   m  = Month (1-12, no leading zero)
#   d  = Release counter for the month (starts at 0)
#
# Examples:
#   v26.2.0  - February 2026, first release
#   v26.2.1  - February 2026, second release
#   v26.12.3 - December 2026, fourth release

set -euo pipefail

# Get current year (last 2 digits) and month
YEAR=$(date +%y)
MONTH=$(date +%-m)  # %-m removes leading zero

# Prefix for this month's versions
PREFIX="v${YEAR}.${MONTH}."

# Find existing tags for this month
EXISTING_TAGS=$(git tag -l "${PREFIX}*" 2>/dev/null | sort -V || echo "")

if [ -z "$EXISTING_TAGS" ]; then
    # No tags for this month, start at 0
    NEXT_COUNTER=0
else
    # Find the highest counter
    HIGHEST=$(echo "$EXISTING_TAGS" | tail -1 | sed "s/${PREFIX}//")
    NEXT_COUNTER=$((HIGHEST + 1))
fi

NEXT_VERSION="${YEAR}.${MONTH}.${NEXT_COUNTER}"

# Output options
case "${1:-}" in
    --tag)
        echo "v${NEXT_VERSION}"
        ;;
    --version)
        echo "${NEXT_VERSION}"
        ;;
    --info)
        echo "Year:    20${YEAR}"
        echo "Month:   ${MONTH}"
        echo "Counter: ${NEXT_COUNTER}"
        echo "Version: ${NEXT_VERSION}"
        echo "Tag:     v${NEXT_VERSION}"
        if [ -n "$EXISTING_TAGS" ]; then
            echo ""
            echo "Existing tags this month:"
            echo "$EXISTING_TAGS" | sed 's/^/  /'
        fi
        ;;
    *)
        echo "${NEXT_VERSION}"
        ;;
esac
