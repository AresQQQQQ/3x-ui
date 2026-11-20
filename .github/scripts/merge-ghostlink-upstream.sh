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

unmerged="$(git diff --name-only --diff-filter=U)"
if [[ -z "$unmerged" ]]; then
    exit 1
fi

# Auto-resolve only known non-runtime housekeeping paths that diverge from upstream:
# - .github/dependabot.yml stays removed so Dependabot version-update PRs stay disabled.
# - docs/ dependency manifests/lockfiles are not built into the panel image and track upstream.
while IFS= read -r path; do
    case "$path" in
        .github/dependabot.yml)
            git rm -f --ignore-unmatch -- "$path" >/dev/null
            ;;
        docs/package.json|docs/pnpm-lock.yaml|docs/pnpm-workspace.yaml)
            if git cat-file -e "$commit:$path" 2>/dev/null; then
                git checkout "$commit" -- "$path"
                git add -- "$path"
            else
                git rm -f --ignore-unmatch -- "$path" >/dev/null
            fi
            ;;
    esac
done <<< "$unmerged"

git diff --name-only --diff-filter=U > "$conflicts"
if [[ ! -s "$conflicts" ]] && [[ -z "$(git ls-files -u)" ]]; then
    exit 0
fi
if [[ -s "$conflicts" ]]; then
    git merge --abort
    exit 3
fi
exit 1
