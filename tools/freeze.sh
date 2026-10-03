#!/usr/bin/env bash
# Freeze the exported API for a release.
# One call writes the record pair for the named version, stages it, and runs
# the gate, so the release PR arrives already judged: the transcript matches
# the tree, the export matches the build, and the version obeys the bump
# policy that tools/check.sh judges.

set -euo pipefail

version=${1:?usage: tools/freeze.sh vX.Y.Z}
if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "FAIL freeze: $version is not vX.Y.Z" >&2
    exit 1
fi

repo_root=$(git rev-parse --show-toplevel 2>/dev/null) || true
if [ -z "$repo_root" ]; then
    echo "FAIL freeze: run this script from inside the keel git repository" >&2
    exit 1
fi
cd "$repo_root"

doc="api/${version}.txt"
export_data="api/${version}.export"

if [ -e "$doc" ] || [ -e "$export_data" ]; then
    echo "FAIL freeze: a record for ${version} already exists" >&2
    exit 1
fi

# The record must move forward. Records on disk are the released history.
# sort -V orders vX.Y.Z correctly, so v0.10.0 sorts after v0.9.0.
prev=$(find api -maxdepth 1 -name 'v*.txt' -printf '%f\n' 2>/dev/null \
        | sed 's/\.txt$//' | sort -V | tail -1 || true)
if [ -n "$prev" ] && [ "$(printf '%s\n%s\n' "$prev" "$version" | sort -V | head -1)" != "$prev" ]; then
    echo "FAIL freeze: ${version} is not strictly newer than ${prev}, a release moves forward" >&2
    exit 1
fi

# The record must describe the commit the release carries. Unstaged or
# untracked Go files would make the pair lie about the tree it freezes.
git update-index -q --refresh || true
if ! git diff --quiet -- '*.go' go.mod go.sum; then
    echo "FAIL freeze: unstaged Go changes above, commit or stage them so the record matches the release" >&2
    git diff --name-only -- '*.go' go.mod go.sum >&2
    exit 1
fi
if [ -n "$(git ls-files -o --exclude-standard -- '*.go' go.mod go.sum)" ]; then
    git ls-files -o --exclude-standard -- '*.go' go.mod go.sum >&2
    echo "FAIL freeze: untracked Go files above would escape the record, add or remove them first" >&2
    exit 1
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# The transcript recipe must stay in step with regenerate_api_doc in
# tools/check.sh: one header line per package, go doc -all under the frozen
# context, one blank line between packages. The recorded packages are every
# package of the module except the tools package, matching recorded_packages
# there. Both files are written to a staging directory first, so a failure
# leaves no half record behind.
: > "$tmp/doc.txt"
while IFS= read -r pkg; do
    printf '########## %s\n' "$pkg" >> "$tmp/doc.txt"
    GOOS=linux GOARCH=amd64 go doc -all "$pkg" >> "$tmp/doc.txt"
    printf '\n' >> "$tmp/doc.txt"
done < <(go list ./... | grep -v '/tools')
if [ ! -s "$tmp/doc.txt" ]; then
    echo "FAIL freeze: no packages were documented, refusing to write an empty record" >&2
    exit 1
fi

# Keep the pin in step with tools/check.sh, which judges the pair with the
# same tool build. GOBIN keeps the tool out of go.mod.
apidiff_version="v0.0.0-20260908205506-85c1c2202aba"
mkdir -p "$tmp/bin"
GOBIN="$tmp/bin" go install "golang.org/x/exp/cmd/apidiff@${apidiff_version}"
GOOS=linux GOARCH=amd64 "$tmp/bin/apidiff" -m -w "$tmp/data.export" "$(go list -m)"

# Incompatible changes are legal on the right bump, not hidden. Print what the
# build carries beyond the previous release so the author sees the size of the
# move before the gate judges it. A package the tree drops is invisible to
# apidiff, so the previous record's header list is diffed against the fresh
# one here. tools/check.sh holds the final verdict on both.
if [ -n "$prev" ]; then
    breaks=$(GOOS=linux GOARCH=amd64 "$tmp/bin/apidiff" -incompatible -m "api/${prev}.export" "$(go list -m)" || true)
    removed=$(comm -23 \
        <(git show "HEAD:api/${prev}.txt" | sed -n 's/^########## //p' | sort -u) \
        <(sed -n 's/^########## //p' "$tmp/doc.txt" | sort -u))
    if [ -n "$breaks" ]; then
        printf 'note: incompatible changes against %s\n%s\n' "$prev" "$breaks" >&2
    fi
    if [ -n "$removed" ]; then
        printf 'note: packages dropped since %s\n%s\n' "$prev" "$removed" >&2
    fi
    if [ -n "$breaks" ] || [ -n "$removed" ]; then
        printf 'note: tools/check.sh refuses these on a patch bump, see the bump policy there\n' >&2
    fi
fi

mkdir -p api
mv "$tmp/doc.txt" "$doc"
mv "$tmp/data.export" "$export_data"
git add "$doc" "$export_data"
printf 'froze %s: %s and %s staged, running the gate\n' "$version" "$doc" "$export_data"

exec tools/check.sh
