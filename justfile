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

# The job binary's dynamic loader in a release: the x86_64 psABI's path, where
# FHS glibc distributions have it. Other builds keep the dev shell's Go
# default, the Nix store's glibc, which the dev shell's libkrun is linked
# against (docs/development.md).
release_ldso := "/lib64/ld-linux-x86-64.so.2"

# LDSO, when set, is the job binary's dynamic loader.
# Build the job binary and the agent into internal/payload/, for embedding.
payload ldso="":
    CGO_ENABLED=0 go build {{ build_flags }} -ldflags '{{ if ldso == "" { "" } else { "-I " + ldso } }}' -o internal/payload/runner-microvm-job ./cmd/runner-microvm-job
    CGO_ENABLED=0 go build {{ build_flags }} -o internal/payload/runner-microvm-agent ./cmd/runner-microvm-agent

# Build bin/runner-microvm, job binary and agent embedded; it says version dev.
build: (plugin "bin/runner-microvm")

# Build the plugin, payload embedded, to OUT, passing LDFLAGS to the linker
# and LDSO to payload: the one build definition `build` and `release-assets`
# share.
[private]
plugin out ldflags="" ldso="": (payload ldso)
    CGO_ENABLED=0 go build {{ build_flags }} -tags payload -ldflags '{{ ldflags }}' -o '{{ out }}' ./cmd/runner-microvm

# The variable the version is set in (docs/development.md).
version_var := "github.com/chrishoage/forgejo-runner-plugin-microvm/internal/plugin.version"

# Where release-assets writes what a release attaches.
release_dir := "dist"

# VERSION is passed as is (1.2.3, no v), and dist/ is replaced each time. It
# fails unless the embedded job binary names release_ldso as its loader.
# Build what a release attaches into dist/: the versioned binary and SHA256SUMS.
release-assets version:
    rm -rf '{{ release_dir }}'
    {{ just_executable() }} plugin '{{ release_dir }}/runner-microvm-{{ version }}-linux-amd64' '-X {{ version_var }}={{ version }}' '{{ release_ldso }}'
    interp=$(readelf -l internal/payload/runner-microvm-job | sed -n 's/.*Requesting program interpreter: \(.*\)]$/\1/p') && test "$interp" = '{{ release_ldso }}' || { echo "the job binary's loader is '$interp', not {{ release_ldso }}" >&2; exit 1; }
    cd '{{ release_dir }}' && sha256sum 'runner-microvm-{{ version }}-linux-amd64' > SHA256SUMS

# Print the release notes for the commits up to TO since the v* tag before it
# (all of them if there is none): the forgejo-runner version the tests run
# against and the library minimums, both as committed at TO, then
# `cog changelog`, which lists the types cog.toml keeps.
release-notes to="HEAD":
    #!/usr/bin/env bash
    set -euo pipefail
    to='{{ to }}'
    prev=$(git describe --tags --abbrev=0 --match 'v*' --exclude "$to" "$to" 2>/dev/null || true)
    gomod=$(mktemp --suffix=.mod)
    trap 'rm -f "$gomod"' EXIT
    git show "$to:go.mod" > "$gomod"
    runner=$(go mod edit -json "$gomod" | jq -r '.Require[] | select(.Path | startswith("code.forgejo.org/forgejo/runner/")) | .Version')
    echo "Tested with forgejo-runner $runner"
    echo
    git show "$to:nix/lib-minimums.json" | jq -r '"Requires libkrun ≥ \(.libkrun), libkrunfw ≥ \(.libkrunfw)"'
    echo
    cog changelog "$prev..$to"

# VERSION defaults to what `cog bump --auto` proposes; a leading v is dropped.
# Open the version PR: write VERSION to the VERSION file on a branch
# release/v<VERSION>, commit it, push it, open a PR showing the notes, and
# switch back to the branch it started on.
bump version="":
    #!/usr/bin/env bash
    set -euo pipefail
    version='{{ version }}'
    if [ -z "$version" ]; then
        version=$(cog bump --auto --dry-run)
    fi
    version="${version#v}"
    branch="release/v$version"
    notes=$({{ just_executable() }} release-notes)
    git switch -c "$branch"
    echo "$version" > VERSION
    git add VERSION
    git commit -m "chore(release): v$version"
    git push -u origin "$branch"
    gh pr create --base main --head "$branch" --title "chore(release): v$version" --body "$notes"
    git switch -

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
# embedded payload, passing FLAGS on to go test (CI passes -v). Needs
# /dev/kvm and the dev shell's libraries.
test-kvm *flags: payload
    go test -race -tags kvm,payload {{ flags }} ./...

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
