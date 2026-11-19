# SPDX-License-Identifier: GPL-3.0-or-later
#
# Every routine command is a recipe, so the maintainer, agents and CI run the
# same commands. Run them inside the dev shell: `nix develop -c just <recipe>`.

# Licenses a dependency may carry (docs/adr/0001), as go-licenses names them.
allowed_licenses := "MIT,BSD-2-Clause,BSD-3-Clause,Apache-2.0,MPL-2.0,LGPL-2.1,LGPL-3.0,GPL-3.0,ISC,0BSD,Unlicense,CC0-1.0"

# List the recipes.
default:
    @just --list

# Build the runner-microvm binary into bin/.
build:
    CGO_ENABLED=0 go build -trimpath -o bin/runner-microvm ./cmd/runner-microvm

# Apply the formatters (gofumpt, goimports).
fmt:
    golangci-lint fmt

# Lint, formatting included. Any finding fails.
lint:
    golangci-lint run

# Run the unit tests with the race detector.
test:
    go test -race ./...

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
    if git rev-parse -q --verify "$from^" >/dev/null; then
        exec cog check "$range"
    fi
    # cocogitto 7.0.0 checks the whole history, root included, when a range
    # starts at the root commit; verify each commit in the range itself.
    status=0
    for c in $(git rev-list "$range"); do
        if ! out=$(cog verify "$(git log -1 --format=%B "$c")" 2>&1); then
            printf 'commit %s is not a Conventional Commit:\n%s\n' "$c" "$out" >&2
            status=1
        fi
    done
    exit "$status"

# Everything a PR must pass.
check: build lint test licenses vuln lint-commits
