# Development

How the project is built, and why it is built this way.

## Quick start

With only Nix installed (flakes enabled):

```sh
nix develop -c just build   # bin/runner-microvm
nix develop -c just check   # everything a PR must pass
```

With direnv, `.envrc` (`use flake`) enters the dev shell on `cd`; then run `just <recipe>` directly. `just` with no recipe lists them all.

## Module and layout

- The module path is `github.com/chrishoage/forgejo-runner-plugin-microvm`: it matches where the code lives, and a vanity domain is one more thing to own.
- The binary is named `runner-microvm`, matching the naming of other runner plugins; the repository name already carries "forgejo".
- The placeholder `main` lives under `cmd/runner-microvm/`. It is a stand-in: the package layout below the top level belongs to the architecture epic (#3), which may move it.

## Toolchain

- **One Nix flake dev shell**, used by the maintainer, agents and CI: one environment means one set of tool versions.
- **nixpkgs follows `nixos-unstable`:** it carries current Go and lint tools, and a stable channel lags.
- **x86_64 Linux only:** the flake defines the dev shell for `x86_64-linux` alone, because GitHub's x86 Linux runners provide KVM; arm64 is out of scope.
- **The flake selects `go_1_26` explicitly,** so a change to nixpkgs' default `go` can't silently change the Go minor version. The shell's Go tools (golangci-lint, go-licenses, govulncheck) are built with that same Go, so they agree with `go` on the standard library; nixpkgs would otherwise build them with its latest Go.
- **`go.mod` states the language minimum `go 1.26`, with no `toolchain` line, and the dev shell sets `GOTOOLCHAIN=local`,** so Go never downloads a toolchain of its own: the flake is the only source of Go.
- **Go patch releases arrive through `flake.lock` updates.** A minor bump (1.27) is a deliberate change of `go.mod` and the flake together.
- **The dev shell carries every tool the project's checks need:** Go, just, golangci-lint, go-licenses, govulncheck and cocogitto. It also carries a C compiler, because `go test -race` needs cgo even though the binary builds without it (`CGO_ENABLED=0`).
- **`.envrc` contains `use flake`,** so direnv users get the shell on `cd`.
- **`just` is the task runner:** every routine command is a recipe, so the maintainer, agents and CI run the same commands.
