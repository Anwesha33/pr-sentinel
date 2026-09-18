#!/usr/bin/env bash
# Regenerates each fixture's pr.diff from its base/ and head/ trees.
#
# The diff is committed rather than generated at eval time so that a review can
# be scored on a machine with no git, and so that a change to a fixture shows up
# as a reviewable diff in the pull request that made it.
set -euo pipefail
cd "$(dirname "$0")/fixtures"

for d in */; do
  name="${d%/}"
  # git diff --no-index exits 1 when the trees differ, which is the normal case.
  git diff --no-index --no-color "$name/base" "$name/head" > /tmp/raw.diff || true
  # Rewrite base/ and head/ out of the paths so the diff looks like a real PR
  # against the repository root.
  sed -e "s|a/$name/base/|a/|g" -e "s|b/$name/head/|b/|g" \
      -e "s| $name/base/| a/|g" -e "s| $name/head/| b/|g" \
      /tmp/raw.diff > "$name/pr.diff"
  echo "regenerated $name/pr.diff"
done
