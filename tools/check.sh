#!/usr/bin/env bash
# Keel quality gate.
# One script runs every check so the workstation and CI never drift apart.
# It exits 1 and names the first failed check.

set -euo pipefail

# Run from the repository root so every path below works from any cwd inside it.
repo_root=$(git rev-parse --show-toplevel 2>/dev/null) || true
if [ -z "$repo_root" ]; then
    echo "FAIL setup: run this script from inside the keel git repository" >&2
    exit 1
fi
cd "$repo_root"

fail() {
    echo "FAIL $1: $2" >&2
    exit 1
}

header() {
    echo "== check $1 =="
}

passed() {
    echo "ok"
}

# The content scans skip two tracked files, matched by exact path.
# This script must hold the banned names so the greps have patterns to search for.
# The ignore list must name the paths it keeps out of the repository.
# The scanner therefore never inspects either file, and nothing else is exempt.
excluded_paths=("tools/check.sh" ".gitignore")

# Pin the staticcheck version so every machine audits with the same tool build.
staticcheck_version="v0.8.1"

# Pin the apidiff version the way staticcheck is pinned, so every machine
# compares the API with one tool build. The tool never joins go.mod, because
# the module's requirement set is frozen.
apidiff_version="v0.0.0-20260908205506-85c1c2202aba"

# Two records freeze the exported API, and both live under api/.
# api/v0.1.0.txt is the readable go doc -all transcript, which a diff can show
# and a reviewer can read. api/v0.1.0.export is the binary export data that the
# apidiff tool compares against. apidiff reads no transcript and a human reads
# no export data, so the two formats stay separate on purpose.
api_doc="api/v0.1.0.txt"
api_export="api/v0.1.0.export"

# Both records are taken from one frozen build context, linux/amd64, which is
# also the context CI runs on. The transcript is generated under it, so the same
# bytes come out on any host. The conventions check keeps every file inside that
# context, so no other context can grow the exported surface unseen.
frozen_goos="linux"
frozen_goarch="amd64"

# Consumer product names must never appear in tracked files.
consumer_re='thutapi|ajilamu|reprise|dev-diary'

# Planning artifacts must never appear in tracked files.
# The patterns cover task ids, phase ids, planning file names, the section mark,
# and numbered notes about invariants or rounds.
plan_re='\bT[0-9]+(\.[0-9]+)?[a-z]?\b|\bP[0-9]+\b|plan\.md|project\.md|handoff\.md|§|invariant [0-9]+|round[0-9]+'

# Tracked files feed the two content scans below.
mapfile -d '' -t tracked_files < <(git ls-files -z)
scanned_files=()
for path in "${tracked_files[@]}"; do
    keep=1
    for excluded in "${excluded_paths[@]}"; do
        if [ "$path" = "$excluded" ]; then
            keep=0
            break
        fi
    done
    # A tracked file missing from the worktree has no content to scan.
    if [ "$keep" = 1 ] && [ -f "$path" ]; then
        scanned_files+=("$path")
    fi
done

# Check 1: every Go file stays gofmt clean.
header "1/10 gofmt"
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
    printf '%s\n' "$unformatted"
    fail "1/10 gofmt" "files above need gofmt -w"
fi
passed

# A fresh module has no packages yet, and the compiled checks below need one.
# go list may fail or warn on an empty module, so capture its output without failing.
packages=$(go list ./... 2>/dev/null || true)

if [ -n "$packages" ]; then
    # Check 2: the Go vet suite passes.
    header "2/10 go vet"
    go vet ./... || fail "2/10 go vet" "go vet found problems"
    passed

    # Check 3: staticcheck passes at the pinned version.
    header "3/10 staticcheck"
    go run "honnef.co/go/tools/cmd/staticcheck@${staticcheck_version}" ./... \
        || fail "3/10 staticcheck" "staticcheck found problems"
    passed

    # Check 4: every package builds without cgo.
    header "4/10 build"
    # A cgo-only directory vanishes from ./... under CGO_ENABLED=0.
    # go build then warns and exits 0, so compare the package lists as well.
    packages_cgo=$(CGO_ENABLED=1 go list ./... 2>/dev/null || true)
    packages_nocgo=$(CGO_ENABLED=0 go list ./... 2>/dev/null || true)
    missing_packages=$(comm -23 <(sort <<<"$packages_cgo") <(sort <<<"$packages_nocgo"))
    CGO_ENABLED=0 go build ./... || fail "4/10 build" "build failed with CGO disabled"
    if [ -n "$missing_packages" ]; then
        printf '%s\n' "$missing_packages"
        fail "4/10 build" "packages above disappear when cgo is disabled"
    fi
    passed

    # Check 5: the test suite passes under the race detector.
    header "5/10 test"
    go test -race ./... || fail "5/10 test" "tests failed under the race detector"
    passed
else
    echo "== checks 2 to 5 (go vet, staticcheck, build, test) =="
    echo "no packages yet, skipping"
fi

# Both content scans pass -I to grep so they judge file content and not file names.
# Grep treats a file holding a NUL byte as binary and as if it held no matches.
# The binary fixtures carry every short byte sequence by chance, and chance is not a citation.
# A text file named *.bin holds real words, and grep still scans it.
# Check 6: no consumer name in a tracked file.
header "6/10 consumer names"
if [ "${#scanned_files[@]}" -gt 0 ]; then
    hits=$(grep -l -i -E -I -e "$consumer_re" -- "${scanned_files[@]}" || true)
    if [ -n "$hits" ]; then
        printf '%s\n' "$hits"
        fail "6/10 consumer names" "tracked files above name a consumer"
    fi
fi
passed

# Check 7: no planning reference in a tracked file.
header "7/10 plan references"
if [ "${#scanned_files[@]}" -gt 0 ]; then
    hits=$(grep -l -i -E -I -e "$plan_re" -- "${scanned_files[@]}" || true)
    if [ -n "$hits" ]; then
        printf '%s\n' "$hits"
        fail "7/10 plan references" "tracked files above cite planning"
    fi
fi
passed

# Check 8: only packages built to wrap sqlite may depend on its driver.
header "8/10 sqlite edge"
if [ -z "$packages" ]; then
    echo "no packages yet, skipping"
else
    while IFS= read -r pkg; do
        case "$pkg" in
            */sqlite | */sqlite/* | */sqlitestore | */sqlitestore/*) continue ;;
        esac
        # grep -q can end the pipe early, which kills go list with SIGPIPE
        # under pipefail, so a real hit would read as clean.
        # Capture the list first and match it without an early exit.
        deps=$(go list -deps "$pkg") || fail "8/10 sqlite edge" "go list -deps ${pkg} failed"
        case $'\n'"${deps}"$'\n' in
            *$'\n'modernc.org/sqlite$'\n'*) fail "8/10 sqlite edge" "package ${pkg} depends on modernc.org/sqlite" ;;
        esac
    done <<< "$packages"
    passed
fi

# Check 9: the four Go conventions that gofmt, vet and staticcheck cannot see.
header "9/10 conventions"
go run ./tools/conventions || fail "9/10 conventions" "the convention breaches above must be fixed"
passed

# regenerate_api_doc writes a go doc -all transcript of every module package to
# the path in $1. The package list is sorted, and each package contributes a
# header line, so a regeneration on an unchanged tree is byte for byte the same.
# Both calls run under the frozen build context named above, so the transcript
# is host independent.
regenerate_api_doc() {
    local dest=$1
    local pkg
    : > "$dest"
    while IFS= read -r pkg; do
        printf '########## %s\n' "$pkg" >> "$dest"
        GOOS="$frozen_goos" GOARCH="$frozen_goarch" go doc -all "$pkg" >> "$dest"
        printf '\n' >> "$dest"
    done < <(GOOS="$frozen_goos" GOARCH="$frozen_goarch" go list ./... | LC_ALL=C sort)
}

# Check 10: the exported API matches both frozen records, taken from the one
# frozen build context named above, linux/amd64, which CI also runs on. The
# transcript half regenerates under that context, so the same bytes come out on
# any host. The conventions check keeps every file inside that context, so a file
# from another context cannot grow the surface the records never see. The
# transcript catches a signature or a doc change a reader can see. The baseline
# catches a change apidiff classifies as compatible or not.
header "10/10 api"
api_tmp=$(mktemp -d)
trap 'rm -rf "$api_tmp"' EXIT
regenerate_api_doc "$api_tmp/v0.1.0.txt"
if ! diff -u "$api_doc" "$api_tmp/v0.1.0.txt"; then
    fail "10/10 api" "the exported API differs from ${api_doc}, see the diff above"
fi
# The pinned apidiff refuses to run when GOOS is set for its own build, so build
# it once for the host and run that binary under the frozen context. GOBIN keeps
# the tool out of go.mod.
mkdir -p "$api_tmp/bin"
GOBIN="$api_tmp/bin" go install "golang.org/x/exp/cmd/apidiff@${apidiff_version}" \
    || fail "10/10 api" "apidiff could not be installed at ${apidiff_version}"
api_report=$(GOOS="$frozen_goos" GOARCH="$frozen_goarch" "$api_tmp/bin/apidiff" -m "$api_export" "$(go list -m)") \
    || fail "10/10 api" "apidiff could not compare against ${api_export}"
if [ -n "$api_report" ]; then
    printf '%s\n' "$api_report"
    fail "10/10 api" "apidiff reports a change against ${api_export}, see the report above"
fi
passed

echo "all checks passed"
