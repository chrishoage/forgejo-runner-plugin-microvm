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
- The package layout, three programs under `cmd/` and everything else under `internal/`, is in [`docs/architecture.md`](architecture.md#packages).

## Toolchain

- **One Nix flake dev shell**, used by the maintainer, agents and CI: one environment means one set of tool versions.
- **nixpkgs follows `nixos-unstable`:** it carries current Go and lint tools, and a stable channel lags.
- **x86_64 Linux only:** the flake defines the dev shell for `x86_64-linux` alone, because GitHub's x86 Linux runners provide KVM; arm64 is out of scope.
- **The flake selects `go_1_26` explicitly,** so a change to nixpkgs' default `go` can't silently change the Go minor version. The shell's Go tools (golangci-lint, go-licenses, govulncheck) are built with that same Go, so they agree with `go` on the standard library; nixpkgs would otherwise build them with its latest Go.
- **`go.mod` states the language minimum `go 1.26` (written `1.26.0` once a dependency requires that form), with no `toolchain` line, and the dev shell sets `GOTOOLCHAIN=local`,** so Go never downloads a toolchain of its own: the flake is the only source of Go.
- **Go patch releases arrive through `flake.lock` updates.** A minor bump (1.27) is a deliberate change of `go.mod` and the flake together.
- **The dev shell carries every tool the project's checks need:** Go, just, golangci-lint, go-licenses, govulncheck, cocogitto, and buf with protoc-gen-go and protoc-gen-go-grpc. It also carries a C compiler, because `go test -race` needs cgo even though the binary builds without it (`CGO_ENABLED=0`).
- **`.envrc` contains `use flake`,** so direnv users get the shell on `cd`.
- **`just` is the task runner:** every routine command is a recipe, so the maintainer, agents and CI run the same commands.

## Protocols and codegen

The three protocols are `.proto` files under `proto/`; `docs/architecture.md` (Protocols) says what each carries.

```sh
nix develop -c just generate          # regenerate the Go code from proto/
nix develop -c just check-generated   # fail if the committed code is stale
nix develop -c just lint              # golangci-lint, then buf lint
```

- **`buf` with local plugins (`protoc-gen-go`, `protoc-gen-go-grpc`) from the dev shell, and no Buf Schema Registry.** One tool generates and lints, with no account or network. `buf.yaml` names the module (`proto/`) and the lint rules; `buf.gen.yaml` the plugins and output.
- **`buf lint` runs at its STANDARD rules, in `just lint`.** Each `.proto` file's directory mirrors its package, which has a version suffix (`runnermicrovm/agent/v1`).
- **Go import paths come from buf's managed mode, so the `.proto` files carry no `go_package`.** The agent and job protocols are generated under `internal/gen/`; the VM child spec under `internal/job/internal/gen/`, so only the job binary can import it.
- **Generated code is committed,** so `go build` and `go install` need no codegen.
- **`just generate` deletes the generated directories and regenerates them,** so a removed `.proto` file leaves nothing behind.
- **`just check-generated` generates into a temporary directory and compares,** as part of `just check` and as CI's `generate` job. Stale generated code fails the PR, and the check works the same before and after `git add`.
- **The generated code needs Go modules at least as new as the generators:** `google.golang.org/protobuf` at least `protoc-gen-go`'s version, and a `google.golang.org/grpc` that supports `protoc-gen-go-grpc`'s output. A flake update that bumps a generator may need a module bump in the same PR.

## Libraries and KVM

runner-microvm runs each job's VM with libkrun, which loads its guest kernel from libkrunfw.

### Minimum versions

The minimums live in `nix/lib-minimums.json`. They apply to the dev shell, CI and the operator docs alike.

- **libkrun 1.19.8 or later, built with block and network support.**
  - Older 1.x releases truncate a raw disk image when the guest discards at its end (libkrun #949), which corrupts read-write disks.
  - Without block and network support there are no disks and no job network.
- **libkrunfw 5.6.4 or later:** the version proven by the prototype. Older releases have not been checked for the guest's needs: file-backed EROFS mounts, and overlays built with `lowerdir+`.
- **The library majors are `libkrun.so.1` and `libkrunfw.so.5`.** The project targets the libkrun 1.x API.

### Overlays

- **libkrun is installed by the operator and never bundled.** The dev shell provides it only for development and CI.
- **`nix/libkrun-overlay.nix` overlays each library only while nixpkgs' version is older than its minimum.** As soon as nixpkgs catches up, the flake uses nixpkgs' package. The overlays retire themselves, so a nixpkgs bump can't leave a stale override in force. An active overlay provides exactly the minimum version. Whether each one is active depends on the pinned nixpkgs; #26 records nixpkgs' versions when it was filed.
- **libkrun's block and network support are switched on in either case.**
- **A flake check (`checks.x86_64-linux.libVersions`) asserts that each resolved version is at least its minimum, and that libkrun is built with block and network support,** so a regression fails `nix flake check`. The assertion is evaluated, so `nix flake check --no-build` shows it too.
- **Renovate does not bump the overlays.** libkrun is Rust, so a version change also changes its vendored-crate hash, and an unattended bump would produce a broken PR. Bumping is a manual change of the version and hashes in the overlay.
- **#26 tracks removing the overlays** once nixpkgs meets both minimums. The overlay links it. When it closes, the dead override code is deleted.

### Library location

- **The dev shell exports `RUNNER_MICROVM_LIBDIR`:** the one directory holding both libraries, which the plugin will later read to find them (#1).
- **`just check-libs` checks that directory.** `libkrun.so.1` and `libkrunfw.so.5` must resolve to real files whose versioned names are at least the minimums. It prints both versions.

### KVM in CI

- **The `kvm` job on `ubuntu-24.04` proves a GitHub x86 runner can use `/dev/kvm`.** It does four things:
  1. Adds a udev rule granting `/dev/kvm` read-write to everyone (`KERNEL=="kvm", GROUP="kvm", MODE="0666"`).
  2. Reloads udev.
  3. Opens `/dev/kvm` read-write as the runner user.
  4. Runs `nix develop -c just check-libs`, then `nix flake check`, so a library below its minimum also fails CI.
- **The job boots no VM:** booting is plugin behaviour, which belongs to the architecture (#3) and first-workflow (#4) epics.
- **It uses the same Nix install and store cache as the other jobs.** The cache matters here, because the libkrunfw overlay builds a kernel. The cache key includes `nix/`, so a change to the overlays builds and saves a fresh store.
- **`setup-nix` roots the dev shell with `nix develop --profile`,** because `nix develop` makes no GC root and the cache action collects garbage before it saves, which would delete the libraries and make the next run rebuild them.
