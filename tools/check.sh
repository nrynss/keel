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

# The exported API is frozen as one pair of records per release, both under
# api/: vX.Y.Z.txt is the readable go doc -all transcript, and vX.Y.Z.export
# is the binary export data the apidiff tool compares against. The guard
# below hardcodes no record name, so it judges whichever pair a release
# stages. apidiff reads no transcript and a human reads no export data, so
# the formats stay separate on purpose.

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

# Check 1: every Go file the commit carries stays gofmt clean at the index
# bytes. A commit carries the index, not the working tree, so the check
# materializes exactly the tracked Go files, at their index bytes, into a
# temporary directory and runs gofmt over those copies. It judges nothing
# else: not the worktree copies, and not the gitignored probe files the
# worktree happens to hold.
header "1/14 gofmt"
gofmt_tmp=$(mktemp -d)
api_tmp=
trap 'rm -rf "$gofmt_tmp" "$api_tmp"' EXIT
git ls-files -z -- '*.go' | git checkout-index -z --prefix="$gofmt_tmp/" --stdin
if ! unformatted=$(
    cd "$gofmt_tmp"
    find . -type f -print0 | xargs -0 -r gofmt -l | sed 's|^\./||'
); then
    fail "1/14 gofmt" "gofmt could not read the indexed copies"
fi
if [ -n "$unformatted" ]; then
    printf '%s\n' "$unformatted"
    fail "1/14 gofmt" "files above need gofmt -w"
fi
passed

# A fresh module has no packages yet, and the compiled checks below need one.
# go list may fail or warn on an empty module, so capture its output without failing.
packages=$(go list ./... 2>/dev/null || true)

if [ -n "$packages" ]; then
    # Check 2: the Go vet suite passes.
    header "2/14 go vet"
    go vet ./... || fail "2/14 go vet" "go vet found problems"
    passed

    # Check 3: staticcheck passes at the pinned version.
    header "3/14 staticcheck"
    go run "honnef.co/go/tools/cmd/staticcheck@${staticcheck_version}" ./... \
        || fail "3/14 staticcheck" "staticcheck found problems"
    passed

    # Check 4: every package builds without cgo.
    header "4/14 build"
    # A cgo-only directory vanishes from ./... under CGO_ENABLED=0.
    # go build then warns and exits 0, so compare the package lists as well.
    packages_cgo=$(CGO_ENABLED=1 go list ./... 2>/dev/null || true)
    packages_nocgo=$(CGO_ENABLED=0 go list ./... 2>/dev/null || true)
    missing_packages=$(comm -23 <(sort <<<"$packages_cgo") <(sort <<<"$packages_nocgo"))
    CGO_ENABLED=0 go build ./... || fail "4/14 build" "build failed with CGO disabled"
    if [ -n "$missing_packages" ]; then
        printf '%s\n' "$missing_packages"
        fail "4/14 build" "packages above disappear when cgo is disabled"
    fi
    passed

    # Check 5: the test suite passes under the race detector.
    header "5/14 test"
    go test -race ./... || fail "5/14 test" "tests failed under the race detector"
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
header "6/14 consumer names"
if [ "${#scanned_files[@]}" -gt 0 ]; then
    hits=$(grep -l -i -E -I -e "$consumer_re" -- "${scanned_files[@]}" || true)
    if [ -n "$hits" ]; then
        printf '%s\n' "$hits"
        fail "6/14 consumer names" "tracked files above name a consumer"
    fi
fi
passed

# Check 7: no planning reference in a tracked file.
header "7/14 plan references"
if [ "${#scanned_files[@]}" -gt 0 ]; then
    hits=$(grep -l -i -E -I -e "$plan_re" -- "${scanned_files[@]}" || true)
    if [ -n "$hits" ]; then
        printf '%s\n' "$hits"
        fail "7/14 plan references" "tracked files above cite planning"
    fi
fi
passed

# Third-party modules stay behind named package prefixes.
# Each row is a module path and the import-path globs allowed to depend on it.
# modernc.org/sqlite -> */sqlite, */sqlite/*, */sqlitestore, */sqlitestore/*
# github.com/pelletier/go-toml/v2 -> */config, */config/source, */config/source/*
# github.com/go-pdf/fpdf -> */book, */book/*
# github.com/aws/aws-sdk-go-v2 and github.com/aws/smithy-go -> */mediastore/s3, */mediastore/s3/*
# The driver chain -> the sqlite globs, because the nine modules below are
# accepted only as the dependency chain modernc.org/sqlite itself pulls in.
# Check 8 walks the sqlite row. Check 9 walks the parser row. Check 10 walks the
# pdf row. Check 11 walks both rows of the object storage client, which arrive
# as two modules of one dependency. Check 12 walks every module of the driver
# chain against the sqlite globs.
edge_sqlite_module="modernc.org/sqlite"
edge_sqlite_prefixes=("*/sqlite" "*/sqlite/*" "*/sqlitestore" "*/sqlitestore/*")
edge_parser_module="github.com/pelletier/go-toml/v2"
edge_parser_prefixes=("*/config" "*/config/source" "*/config/source/*")
edge_pdf_module="github.com/go-pdf/fpdf"
edge_pdf_prefixes=("*/book" "*/book/*")
edge_objectstore_modules=("github.com/aws/aws-sdk-go-v2" "github.com/aws/smithy-go")
edge_objectstore_prefixes=("*/mediastore/s3" "*/mediastore/s3/*")
edge_sqlitechain_modules=(
    "github.com/google/uuid"
    "modernc.org/libc"
    "modernc.org/mathutil"
    "modernc.org/memory"
    "github.com/dustin/go-humanize"
    "github.com/mattn/go-isatty"
    "github.com/ncruces/go-strftime"
    "github.com/remyoudompheng/bigfft"
    "golang.org/x/sys"
)
edge_sqlitechain_prefixes=("*/sqlite" "*/sqlite/*" "*/sqlitestore" "*/sqlitestore/*")

# check_edge walks every package against one table row.
# $1 is the check label. $2 is the module. The rest are allowed globs.
# grep -q can end the pipe early, which kills go list with SIGPIPE
# under pipefail, so a real hit would read as clean.
# Capture the list first and match it without an early exit.
check_edge() {
    local label=$1
    local module=$2
    shift 2
    local pkg deps glob allowed dep
    while IFS= read -r pkg; do
        allowed=0
        for glob in "$@"; do
            case "$pkg" in
                $glob)
                    allowed=1
                    break
                    ;;
            esac
        done
        if [ "$allowed" = 1 ]; then
            continue
        fi
        deps=$(go list -deps "$pkg") || fail "$label" "go list -deps ${pkg} failed"
        # A subpackage of the module counts, because the root import path is not
        # always in the dep list. Match an exact module line or that path plus
        # a slash. A sibling module that only shares a prefix is not a hit.
        while IFS= read -r dep; do
            case "$dep" in
                "$module"|"$module"/*)
                    fail "$label" "package ${pkg} depends on ${module}"
                    ;;
            esac
        done <<< "$deps"
    done <<< "$packages"
}

# Check 8: only packages built to wrap sqlite may depend on its driver.
header "8/14 sqlite edge"
if [ -z "$packages" ]; then
    echo "no packages yet, skipping"
else
    check_edge "8/14 sqlite edge" "$edge_sqlite_module" "${edge_sqlite_prefixes[@]}"
    passed
fi

# Check 9: only packages built to parse settings may depend on the parser.
header "9/14 toml edge"
if [ -z "$packages" ]; then
    echo "no packages yet, skipping"
else
    check_edge "9/14 toml edge" "$edge_parser_module" "${edge_parser_prefixes[@]}"
    passed
fi

# Check 10: only the book package may depend on the PDF library.
header "10/14 pdf edge"
if [ -z "$packages" ]; then
    echo "no packages yet, skipping"
else
    check_edge "10/14 pdf edge" "$edge_pdf_module" "${edge_pdf_prefixes[@]}"
    passed
fi

# Check 11: only the object storage backend may depend on its client.
# The dependency arrives as two modules, the SDK and the runtime it is
# generated on, so the row walks both against the same prefixes.
header "11/14 object storage edge"
if [ -z "$packages" ]; then
    echo "no packages yet, skipping"
else
    for edge_objectstore_module in "${edge_objectstore_modules[@]}"; do
        check_edge "11/14 object storage edge" "$edge_objectstore_module" "${edge_objectstore_prefixes[@]}"
    done
    passed
fi

# Check 12: only the packages built to wrap sqlite may depend on the modules
# of the driver's own dependency chain. Those modules are accepted only as
# what modernc.org/sqlite pulls in, never as direct imports, so the row
# walks every one of them against the sqlite globs.
header "12/14 sqlite chain edge"
if [ -z "$packages" ]; then
    echo "no packages yet, skipping"
else
    for edge_sqlitechain_module in "${edge_sqlitechain_modules[@]}"; do
        check_edge "12/14 sqlite chain edge" "$edge_sqlitechain_module" "${edge_sqlitechain_prefixes[@]}"
    done
    passed
fi

# Check 13: the four Go conventions that gofmt, vet and staticcheck cannot see.
header "13/14 conventions"
go run ./tools/conventions || fail "13/14 conventions" "the convention breaches above must be fixed"
passed

# recorded_packages names the packages a transcript record covers, in the
# order the record writes them: every package of the module except the tools
# package, which serves this repository and no consumer. go list orders the
# packages deterministically, so the same tree regenerates the same bytes.
recorded_packages() {
    go list ./... | grep -v '/tools' || true
}

# regenerate_api_doc writes a go doc -all transcript of every recorded package
# to the path in $1. Each package contributes a header line, so a regeneration
# on an unchanged tree is byte for byte the same. The call runs under the
# frozen build context named above, so the transcript is host independent.
regenerate_api_doc() {
    local dest=$1
    local pkg
    : > "$dest"
    while IFS= read -r pkg; do
        printf '########## %s\n' "$pkg" >> "$dest"
        GOOS="$frozen_goos" GOARCH="$frozen_goarch" go doc -all "$pkg" >> "$dest" \
            || fail "14/14 api" "${pkg} could not be documented under the frozen context"
        printf '\n' >> "$dest"
    done < <(recorded_packages)
}

# api_version_lt reports whether version $1 sorts before version $2, both vX.Y.Z.
api_version_lt() {
    local -a a b
    IFS=. read -r -a a <<< "${1#v}"
    IFS=. read -r -a b <<< "${2#v}"
    if [ "${a[0]}" -ne "${b[0]}" ]; then
        [ "${a[0]}" -lt "${b[0]}" ]
    elif [ "${a[1]}" -ne "${b[1]}" ]; then
        [ "${a[1]}" -lt "${b[1]}" ]
    else
        [ "${a[2]}" -lt "${b[2]}" ]
    fi
}

# Check 14: the records under api/ freeze what a release shipped. A release is
# their only writer. Between releases the gate asks one question: did any
# released byte move? A modification or a deletion refuses outright, read from
# both views, because a commit carries the index and not the working tree. The
# surface a release shipped stays intact even while the tree moves on. A new
# package or a new export joins a record at the next release, so an ordinary
# task never touches api/ at all and this check costs nothing there.
#
# The release pair is the one sanctioned addition, and it is judged hard, on
# the index bytes the commit carries. The pair is exactly one transcript and
# one export record of one version, and that version is strictly newer than
# every record already on disk. The transcript must equal a fresh regeneration
# over the recorded packages under the frozen context, and apidiff must find
# the export identical to the build, so the pair is honest about the tree it
# freezes. Incompatible changes then face the bump policy, judged by running
# apidiff of the previous export against the build: while the module is on v0
# a minor bump may break and a patch may not, and from v1 only a major bump
# may. tools/freeze.sh writes the pair and runs this gate, so a release PR
# arrives already judged.
header "14/14 api"
api_tmp=$(mktemp -d)

api_rows=$({
    git diff --name-status --no-renames HEAD -- api/
    git diff --cached --name-status --no-renames HEAD -- api/
} | sort -u)
# An untracked file under api/ refuses too: a generated pair that was never
# staged is a record no reviewer and no CI run will ever see.
api_strays=$(git status --porcelain -- api/ | sed -n 's/^?? //p')
if [ -n "$api_strays" ]; then
    printf '%s\n' "$api_strays"
    fail "14/14 api" "untracked files above sit under api/, stage the release pair or remove the strays"
fi

# Any row that is not an addition moved a released record. History does not
# move, so no exemption exists. A genuinely broken record is fixed the only
# way records are written, by a release.
api_moved=$(printf '%s\n' "$api_rows" | grep -v '^A' | cut -f2- || true)
if [ -n "$api_moved" ]; then
    fail "14/14 api" "a released record changed: ${api_moved}, releases add records and never move them"
fi

api_added=$(printf '%s\n' "$api_rows" | grep '^A' | cut -f2 || true)
if [ -z "$api_added" ]; then
    # Nothing under api/ moved, the ordinary path between releases. The tree
    # may grow, shrink or change freely. The next release picks the surface up.
    passed
else
    api_pair_fail() {
        printf '%s\n' "$api_added"
        fail "14/14 api" "$1"
    }
    if [ "$(printf '%s\n' "$api_added" | grep -c '\.txt$')" -ne 1 ] \
            || [ "$(printf '%s\n' "$api_added" | grep -c '\.export$')" -ne 1 ] \
            || [ "$(printf '%s\n' "$api_added" | wc -l)" -ne 2 ]; then
        api_pair_fail "a release adds exactly one pair, api/vX.Y.Z.txt and api/vX.Y.Z.export, nothing else"
    fi
    api_doc_added=$(printf '%s\n' "$api_added" | grep '\.txt$')
    api_export_added=$(printf '%s\n' "$api_added" | grep '\.export$')
    api_new=${api_doc_added#api/}
    api_new=${api_new%.txt}
    [[ "$api_new" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] \
        || api_pair_fail "the transcript record ${api_doc_added} does not name a vX.Y.Z version"
    api_new_export=${api_export_added#api/}
    api_new_export=${api_new_export%.export}
    [ "$api_new_export" = "$api_new" ] \
        || api_pair_fail "the transcript and the export record name different versions, ${api_new} and ${api_new_export}"

    # The version moves forward. Records at HEAD are the released history.
    api_prev=""
    while IFS= read -r path; do
        case "$path" in
            api/v*.txt)
                v=${path#api/}
                v=${v%.txt}
                if [ -z "$api_prev" ] || api_version_lt "$api_prev" "$v"; then
                    api_prev=$v
                fi
                ;;
        esac
    done < <(git ls-tree HEAD --name-only -- api/)
    if [ -n "$api_prev" ] && ! api_version_lt "$api_prev" "$api_new"; then
        api_pair_fail "the new record ${api_new} is not strictly newer than ${api_prev}, a release moves forward"
    fi

    regenerate_api_doc "$api_tmp/regen.txt"
    git show ":${api_doc_added}" > "$api_tmp/staged.txt" \
        || fail "14/14 api" "the staged ${api_doc_added} could not be read from the index"
    diff -u "$api_tmp/staged.txt" "$api_tmp/regen.txt" \
        || fail "14/14 api" "the staged transcript ${api_doc_added} does not match the regenerated record, see the diff above"

    # The pinned apidiff refuses to build when GOOS is set for its own build, so
    # build it once for the host and run that binary under the frozen context.
    # GOBIN keeps the tool out of go.mod.
    mkdir -p "$api_tmp/bin"
    GOBIN="$api_tmp/bin" go install "golang.org/x/exp/cmd/apidiff@${apidiff_version}" \
        || fail "14/14 api" "apidiff could not be installed at ${apidiff_version}"
    git show ":${api_export_added}" > "$api_tmp/staged.export" \
        || fail "14/14 api" "the staged ${api_export_added} could not be read from the index"
    api_report=$(GOOS="$frozen_goos" GOARCH="$frozen_goarch" "$api_tmp/bin/apidiff" -incompatible -m "$api_tmp/staged.export" "$(go list -m)") \
        || fail "14/14 api" "apidiff could not compare the staged ${api_export_added} against the build"
    if [ -n "$api_report" ]; then
        printf '%s\n' "$api_report"
        fail "14/14 api" "the staged ${api_export_added} does not match the build, regenerate it with tools/freeze.sh"
    fi

    # The bump policy. apidiff of the previous export against the build names
    # every incompatible change the release carries, except one: a package the
    # build drops simply leaves no trace in the new export, and apidiff ignores
    # what the previous record names and the build does not carry. The
    # transcript header lists carry that truth, so a package present in the
    # previous record and absent from the new one joins the breaks report and
    # faces the same policy as any other incompatible change.
    if [ -n "$api_prev" ]; then
        git show "HEAD:api/${api_prev}.txt" | sed -n 's/^########## //p' | sort -u > "$api_tmp/prev-headers.txt"
        sed -n 's/^########## //p' "$api_tmp/regen.txt" | sort -u > "$api_tmp/new-headers.txt"
        api_removed=$(comm -23 "$api_tmp/prev-headers.txt" "$api_tmp/new-headers.txt")
        git show "HEAD:api/${api_prev}.export" > "$api_tmp/previous.export" \
            || fail "14/14 api" "the previous record api/${api_prev}.export is missing from HEAD"
        api_breaks=$(GOOS="$frozen_goos" GOARCH="$frozen_goarch" "$api_tmp/bin/apidiff" -incompatible -m "$api_tmp/previous.export" "$(go list -m)") \
            || fail "14/14 api" "apidiff could not compare ${api_prev} against the build"
        if [ -n "$api_removed" ]; then
            printf '%s\n' "$api_removed"
            api_breaks="${api_breaks}
removed packages above are incompatible and were never reported by apidiff"
        fi
        if [ -n "$api_breaks" ]; then
            IFS=. read -r prev_major prev_minor _ <<< "${api_prev#v}"
            IFS=. read -r new_major new_minor _ <<< "${api_new#v}"
            allowed=0
            if [ "$prev_major" -eq 0 ]; then
                # On v0 a minor bump may break the surface, a patch may not.
                if [ "$new_major" -gt 0 ] || [ "$new_minor" -gt "$prev_minor" ]; then
                    allowed=1
                fi
                api_want="a minor bump"
            else
                # From v1 only a major bump may break the surface.
                [ "$new_major" -gt "$prev_major" ] && allowed=1
                api_want="a major bump"
            fi
            if [ "$allowed" -ne 1 ]; then
                printf '%s\n' "$api_breaks"
                fail "14/14 api" "${api_new} breaks the surface ${api_prev} shipped, an incompatible change needs ${api_want}"
            fi
        fi
    fi
    passed
fi

echo "all checks passed"
