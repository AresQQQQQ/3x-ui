#!/usr/bin/env bash
set -euo pipefail

commit="$1"
conflicts="$2"
[[ "$commit" =~ ^[0-9a-f]{40}$ ]]
if git -c user.name='GhostLink CI' -c user.email='ghostlink-ci@users.noreply.github.com' \
    merge --no-commit --no-ff "$commit"; then
    test -z "$(git ls-files -u)"
    exit 0
fi
git diff --name-only --diff-filter=U > "$conflicts"
if [[ -s "$conflicts" ]]; then
    git merge --abort
    exit 3
fi
exit 1
