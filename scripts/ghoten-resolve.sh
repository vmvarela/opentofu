#!/usr/bin/env bash
# Ghoten Conflict Resolution Helper
# Helps resolve merge conflicts locally and update the rerere cache.
#
# Usage:
#   ./scripts/ghoten-resolve.sh [--test]
#
# This script:
#   1. Creates a test release branch
#   2. Attempts to merge all feature branches
#   3. If conflicts occur, pauses for manual resolution
#   4. After resolution, updates the .rerere-cache directory
#   5. Commits the updated cache for use in CI
#
# Options:
#   --test    Only test merges, don't update rerere cache

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log_info() { echo -e "${BLUE}ℹ️  $1${NC}"; }
log_success() { echo -e "${GREEN}✅ $1${NC}"; }
log_warning() { echo -e "${YELLOW}⚠️  $1${NC}"; }
log_error() { echo -e "${RED}❌ $1${NC}"; }

cd "$REPO_ROOT"

# Check for uncommitted changes
if ! git diff-index --quiet HEAD --; then
    log_error "You have uncommitted changes. Please commit or stash them first."
    exit 1
fi

# Save current branch
ORIGINAL_BRANCH=$(git branch --show-current)

# Enable rerere
git config rerere.enabled true
git config rerere.autoupdate true

# Ensure rerere cache directory exists
mkdir -p .git/rr-cache

# Load existing rerere cache from repo
if [ -d ".rerere-cache" ] && [ "$(ls -A .rerere-cache 2>/dev/null)" ]; then
    log_info "Loading existing rerere cache..."
    cp -r .rerere-cache/* .git/rr-cache/ 2>/dev/null || true
fi

# Feature branches to merge
BRANCHES=(
    "origin/backend/oci"
    "origin/feature/smart-refresh"
    "origin/ghoten/branding"
)

# Fetch latest
log_info "Fetching latest branches..."
git fetch origin --prune

# Create test branch from main
log_info "Creating test branch from origin/main..."
git checkout -B ghoten/resolve-test origin/main

CONFLICTS_FOUND=false
MERGED_BRANCHES=()
CONFLICT_BRANCHES=()

for branch in "${BRANCHES[@]}"; do
    branch_name=$(echo "$branch" | sed 's|origin/||')
    
    # Check if branch exists
    if ! git rev-parse --verify "$branch" >/dev/null 2>&1; then
        log_warning "Branch $branch_name not found, skipping"
        continue
    fi
    
    echo ""
    log_info "Merging $branch_name..."
    
    if git merge --squash "$branch" 2>/dev/null; then
        git commit -m "test: merge $branch_name" --allow-empty 2>/dev/null || true
        log_success "Merged $branch_name successfully"
        MERGED_BRANCHES+=("$branch_name")
    else
        # Check if rerere can auto-resolve
        REMAINING=$(git rerere remaining 2>/dev/null || echo "has-conflicts")
        
        if [ -z "$REMAINING" ]; then
            log_success "Rerere auto-resolved conflicts for $branch_name"
            git add -A
            git commit -m "test: merge $branch_name (auto-resolved)" 2>/dev/null || true
            MERGED_BRANCHES+=("$branch_name (auto-resolved)")
        else
            CONFLICTS_FOUND=true
            CONFLICT_BRANCHES+=("$branch_name")
            log_error "Conflicts in $branch_name"
            echo ""
            echo "Conflicting files:"
            git diff --name-only --diff-filter=U | sed 's/^/  /'
            echo ""
            
            if [ "${1:-}" = "--test" ]; then
                log_info "Test mode: aborting merge"
                git merge --abort 2>/dev/null || true
            else
                echo "Please resolve the conflicts manually, then run:"
                echo "  git add <resolved-files>"
                echo "  git commit -m 'resolve: $branch_name'"
                echo ""
                echo "When done, re-run this script to continue."
                echo "Or run 'git merge --abort' to abort."
                exit 1
            fi
        fi
    fi
done

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "                    SUMMARY"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

if [ ${#MERGED_BRANCHES[@]} -gt 0 ]; then
    log_success "Successfully merged:"
    for b in "${MERGED_BRANCHES[@]}"; do
        echo "  • $b"
    done
fi

if [ ${#CONFLICT_BRANCHES[@]} -gt 0 ]; then
    echo ""
    log_error "Conflicts in:"
    for b in "${CONFLICT_BRANCHES[@]}"; do
        echo "  • $b"
    done
fi

# Update rerere cache if no conflicts and not in test mode
if [ "$CONFLICTS_FOUND" = false ] && [ "${1:-}" != "--test" ]; then
    echo ""
    log_info "Updating rerere cache..."
    
    mkdir -p .rerere-cache
    if [ -d ".git/rr-cache" ] && [ "$(ls -A .git/rr-cache 2>/dev/null)" ]; then
        cp -r .git/rr-cache/* .rerere-cache/ 2>/dev/null || true
        
        # Count entries
        CACHE_COUNT=$(ls .rerere-cache 2>/dev/null | wc -l | tr -d ' ')
        log_success "Rerere cache updated with $CACHE_COUNT entries"
        
        echo ""
        log_info "To save the cache, commit and push:"
        echo "  git checkout ghoten/branding"
        echo "  git add .rerere-cache/"
        echo "  git commit -m 'chore: update rerere cache'"
        echo "  git push origin ghoten/branding"
    fi
fi

# Return to original branch
echo ""
log_info "Returning to $ORIGINAL_BRANCH..."
git checkout "$ORIGINAL_BRANCH"

# Clean up test branch
git branch -D ghoten/resolve-test 2>/dev/null || true

if [ "$CONFLICTS_FOUND" = true ]; then
    exit 1
else
    log_success "All branches can be merged successfully!"
    exit 0
fi
