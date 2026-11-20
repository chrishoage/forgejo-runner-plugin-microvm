# SPDX-License-Identifier: GPL-3.0-or-later
#
# Every routine command is a recipe, so the maintainer, agents and CI run the
# same commands. Run them inside the dev shell: `nix develop -c just <recipe>`.

# Licenses a dependency may carry (docs/adr/0001), as go-licenses names them.
allowed_licenses := "MIT,BSD-2-Clause,BSD-3-Clause,Apache-2.0,MPL-2.0,LGPL-2.1,LGPL-3.0,GPL-3.0,ISC,0BSD,Unlicense,CC0-1.0"

# List the recipes.
default:
    @just --list

# Build flags shared by every binary: reproducible under Nix.
build_flags := "-trimpath -buildvcs=false"

# Build the job binary and the agent into internal/payload/, for embedding.
payload:
    CGO_ENABLED=0 go build {{ build_flags }} -o internal/payload/runner-microvm-job ./cmd/runner-microvm-job
    CGO_ENABLED=0 go build {{ build_flags }} -o internal/payload/runner-microvm-agent ./cmd/runner-microvm-agent

# Build bin/runner-microvm with the job binary and the agent embedded.
build: payload
    CGO_ENABLED=0 go build {{ build_flags }} -tags payload -o bin/runner-microvm ./cmd/runner-microvm

# Apply the formatters (gofumpt, goimports).
fmt:
    golangci-lint fmt

# Lint, formatting included, and the .proto files. Any finding fails. The
# second pass lints the shipped build (CGO_ENABLED=0, -tags payload) and the
# tests that boot VMs (-tags kvm), so files behind those build constraints
# are linted too.
lint: payload
    golangci-lint run
    CGO_ENABLED=0 golangci-lint run --build-tags payload,kvm
    buf lint

# Directories holding only generated code.
generated := "internal/gen internal/job/internal/gen"

# Regenerate the protocols' Go code from proto/.
generate:
    rm -rf {{ generated }}
    buf generate

# Fail when the committed generated code is not what `just generate` makes.
check-generated:
    #!/usr/bin/env bash
    set -euo pipefail
    out=$(mktemp -d)
    trap 'rm -rf "$out"' EXIT
    buf generate --output "$out"
    status=0
    for dir in {{ generated }}; do
        diff -r "$out/$dir" "$dir" || status=1
    done
    if [ "$status" -ne 0 ]; then
        echo "generated code is stale: run 'just generate' and commit the result" >&2
    fi
    exit "$status"

# Run the unit tests with the race detector. No KVM, no root, no payload.
test:
    go test -race ./...

# Run every test, those that boot VMs (`kvm` tag) included, against the
# embedded payload. Needs /dev/kvm and the dev shell's libraries.
test-kvm: payload
    go test -race -tags kvm,payload ./...

# Fail on any dependency whose license is not on the allow-list.
licenses:
    #!/usr/bin/env bash
    set -euo pipefail
    ignore=()
    while read -r pkg; do ignore+=(--ignore "$pkg"); done < <(grep -v -e '^#' -e '^[[:space:]]*$' licenses-ignore.txt || true)
    go-licenses check ./... --allowed_licenses={{ allowed_licenses }} "${ignore[@]}"

# Fail on known vulnerabilities in code the binary reaches.
vuln:
    govulncheck ./...

# Check that every commit in RANGE is a Conventional Commit.
lint-commits range="origin/main..HEAD":
    #!/usr/bin/env bash
    set -euo pipefail
    range='{{ range }}'
    from="${range%%..*}"
    to="${range#*..}"
    for end in "$from" "${to:-HEAD}"; do
        if ! git rev-parse -q --verify "$end^{commit}" >/dev/null; then
            echo "lint-commits: $end in range $range is not a commit" >&2
            exit 1
        fi
    done
    ignored=$(grep -v -e '^#' -e '^[[:space:]]*$' commits-ignore.txt || true)
    commits=$(git rev-list "$range")
    # cog check can't skip commits, and cocogitto 7.0.0 checks the whole
    # history when a range starts at the root commit. Only in those cases,
    # verify each commit in the range itself.
    if git rev-parse -q --verify "$from^" >/dev/null &&
        { [ -z "$ignored" ] || ! grep -qxF -f <(printf '%s\n' "$ignored") <<<"$commits"; }; then
        exec cog check "$range"
    fi
    status=0
    for c in $commits; do
        if grep -qxF "$c" <<<"$ignored"; then
            echo "skipping $c (commits-ignore.txt)"
            continue
        fi
        if ! out=$(cog verify "$(git log -1 --format=%B "$c")" 2>&1); then
            printf 'commit %s is not a Conventional Commit:\n%s\n' "$c" "$out" >&2
            status=1
        fi
    done
    exit "$status"

# Check that libkrun and libkrunfw in RUNNER_MICROVM_LIBDIR meet the minimums.
check-libs:
    #!/usr/bin/env bash
    set -euo pipefail
    dir="${RUNNER_MICROVM_LIBDIR:?is not set: run inside the dev shell}"
    status=0
    for lib in libkrun.so.1 libkrunfw.so.5; do
        name="${lib%%.so.*}"
        want=$(sed -n "s/^ *\"$name\": *\"\([^\"]*\)\".*/\1/p" nix/lib-minimums.json)
        if [ -z "$want" ]; then
            echo "$name: no minimum in nix/lib-minimums.json" >&2; status=1; continue
        fi
        if ! real=$(readlink -e "$dir/$lib") || [ ! -f "$real" ]; then
            echo "$name: $dir/$lib does not resolve to a file" >&2; status=1; continue
        fi
        have="${real##*/"$name".so.}"
        if ! [[ "$have" =~ ^[0-9]+(\.[0-9]+)*$ ]]; then
            echo "$name: $real has no versioned name" >&2; status=1; continue
        fi
        if [ "$(printf '%s\n%s\n' "$want" "$have" | sort -V | head -n1)" != "$want" ]; then
            echo "$name $have is older than the minimum $want ($real)" >&2; status=1; continue
        fi
        echo "$name $have (minimum $want): $real"
    done
    exit "$status"

# Everything a PR must pass.
check: build lint check-generated test licenses vuln lint-commits

# The local harness: `just e2e up`, `just e2e run`, `just e2e down`
# (docs/development.md, The local harness).
mod e2e
