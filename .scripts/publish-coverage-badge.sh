#!/usr/bin/env bash
#
# Writes a shields.io endpoint JSON onto an orphan "badges" branch, so the
# README badge needs no third-party coverage service and no token beyond the
# workflow's own GITHUB_TOKEN.
#
# The checkout leaves no credentials behind (persist-credentials: false, so a
# later step cannot use them), so the token is put on the remote here instead
# of being read from the checkout.
#
# Usage: PCT=81.0 ./.scripts/publish-coverage-badge.sh
set -euo pipefail

PCT="${PCT:?set PCT to the coverage percentage, e.g. 81.0}"
BRANCH="${BRANCH:-badges}"
REMOTE="origin"
if [ -n "${GITHUB_TOKEN:-}" ] && [ -n "${GITHUB_REPOSITORY:-}" ]; then
  REMOTE="https://x-access-token:${GITHUB_TOKEN}@github.com/${GITHUB_REPOSITORY}.git"
fi

# shields' own palette, so the badge reads the way people expect
colour() {
  local n=${1%%.*}
  if   [ "$n" -ge 90 ]; then echo brightgreen
  elif [ "$n" -ge 80 ]; then echo green
  elif [ "$n" -ge 70 ]; then echo yellowgreen
  elif [ "$n" -ge 60 ]; then echo yellow
  elif [ "$n" -ge 50 ]; then echo orange
  else echo red
  fi
}

tmp="$(mktemp -d)"
trap 'git worktree remove --force "$tmp/wt" 2>/dev/null || true; rm -rf "$tmp"' EXIT

if git fetch --quiet "$REMOTE" "$BRANCH" 2>/dev/null; then
  git worktree add --quiet "$tmp/wt" FETCH_HEAD
  git -C "$tmp/wt" checkout --quiet -B "$BRANCH"
else
  echo "==> $BRANCH does not exist yet, creating it"
  git worktree add --quiet --detach "$tmp/wt"
  git -C "$tmp/wt" checkout --quiet --orphan "$BRANCH"
  git -C "$tmp/wt" rm -rqf . 2>/dev/null || true
fi

cat > "$tmp/wt/coverage.json" <<JSON
{
  "schemaVersion": 1,
  "label": "coverage",
  "message": "${PCT}%",
  "color": "$(colour "$PCT")"
}
JSON

git -C "$tmp/wt" add coverage.json
if git -C "$tmp/wt" diff --cached --quiet; then
  echo "==> coverage is still ${PCT}%, nothing to publish"
  exit 0
fi

git -C "$tmp/wt" -c user.name='github-actions[bot]' \
  -c user.email='41898282+github-actions[bot]@users.noreply.github.com' \
  commit --quiet -m "coverage: ${PCT}%"
git -C "$tmp/wt" push --quiet "$REMOTE" "$BRANCH"
echo "==> published coverage ${PCT}% to $BRANCH"
