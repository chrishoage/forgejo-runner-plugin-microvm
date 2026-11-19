# SPDX-License-Identifier: GPL-3.0-or-later
#
# Every routine command is a recipe, so the maintainer, agents and CI run the
# same commands. Run them inside the dev shell: `nix develop -c just <recipe>`.

# List the recipes.
default:
    @just --list

# Build the runner-microvm binary into bin/.
build:
    CGO_ENABLED=0 go build -trimpath -o bin/runner-microvm ./cmd/runner-microvm

# Run go vet.
vet:
    go vet ./...

# Everything a PR must pass.
check: build vet
