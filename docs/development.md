# Development

How the project is built, and why it is built this way.

## Quick start

With only Nix installed (flakes enabled):

```sh
nix develop -c just build      # bin/runner-microvm, job binary and agent embedded
nix develop -c just check      # everything a PR must pass
nix develop -c just test-kvm   # every test, VM-booting ones included (needs /dev/kvm)
```

With direnv, `.envrc` (`use flake`) enters the dev shell on `cd`; then run `just <recipe>` directly. `just` with no recipe lists them all.

## Module and layout

- The module path is `github.com/chrishoage/forgejo-runner-plugin-microvm`: it matches where the code lives, and a vanity domain is one more thing to own.
- The binary is named `runner-microvm`, matching the naming of other runner plugins; the repository name already carries "forgejo".
- The package layout, three programs under `cmd/` and everything else under `internal/`, is in [`docs/architecture.md`](architecture.md#packages).

## Toolchain

- **One Nix flake dev shell**, used by the maintainer, agents and CI: one environment means one set of tool versions.
- **nixpkgs follows `nixos-unstable`:** it carries current Go and lint tools, and a stable channel lags.
- **x86_64 Linux only:** the flake defines the dev shell and the package for `x86_64-linux` alone, because GitHub's x86 Linux runners provide KVM; arm64 is out of scope.
- **The flake selects `go_1_26` explicitly,** so a change to nixpkgs' default `go` can't silently change the Go minor version. The shell's Go tools (golangci-lint, go-licenses, govulncheck) are built with that same Go, so they agree with `go` on the standard library; nixpkgs would otherwise build them with its latest Go.
- **`go.mod` states the language minimum `go 1.26` (written `1.26.0` once a dependency requires that form), with no `toolchain` line, and the dev shell sets `GOTOOLCHAIN=local`,** so Go never downloads a toolchain of its own: the flake is the only source of Go.
- **Go patch releases arrive through `flake.lock` updates.** A minor bump (1.27) is a deliberate change of `go.mod` and the flake together.
- **The dev shell carries every tool the project's checks need:** Go, just, golangci-lint, go-licenses, govulncheck, cocogitto, nix-update (for `just vendor-hash`), git, and buf with protoc-gen-go and protoc-gen-go-grpc. git is there for the commit check, for the protocol test, whose job executor clones actions with git and whose job repository `git http-backend` serves, and for the harness's `run`, which pushes with it, so none depends on the host's git. It also carries e2fsprogs, for the plugin's one host dependency, `mkfs.ext4`. It also carries a C compiler, because `go test -race` needs cgo even though the binary builds without it (`CGO_ENABLED=0`).
- **`.envrc` contains `use flake`,** so direnv users get the shell on `cd`.
- **`just` is the task runner:** every routine command is a recipe, so the maintainer, agents and CI run the same commands.

## Building and the payload

`runner-microvm` embeds the job binary and the agent (`docs/architecture.md`, The embedded payload).

```sh
nix develop -c just payload               # internal/payload/runner-microvm-{job,agent}
nix develop -c just build                 # runs payload, then bin/runner-microvm with -tags payload
nix develop -c just release-assets 1.2.3  # dist/runner-microvm-1.2.3-linux-amd64 and dist/SHA256SUMS
```

- **`just build` always builds the payload first,** then the plugin with `-tags payload`. Every binary builds with `CGO_ENABLED=0 -trimpath -buildvcs=false`, so the plugin binary is reproducible under Nix.
- **`internal/payload/embedded.go` (tag `payload`) embeds the two files by exact name,** so a build with the tag but without the payload fails to compile. Without the tag, `internal/payload/stub.go` returns `ErrNoPayload`, so `go build ./...`, `go vet` and `go test` need no payload.
- **`just lint` builds the payload, then lints twice:** once as `go test` builds (cgo on, no tags), and once as the binaries ship (`CGO_ENABLED=0`, `-tags payload`), with the `kvm` tag added so the tests that boot VMs are linted too. One pass leaves the other's files unlinted: `embedded.go` hid an unused variable that way. A second pass costs about a second, and the payload build is cached after `just build`; golangci-lint's `build-tags` setting would replace one view instead of adding the other.
- **The built payload is ignored by git and never committed.**
- **The version is a package variable, `version` in `internal/plugin`, set with `-ldflags -X`, and `dev` when unset** (#124). `just build`, the tests and every non-release build say `dev`, so a build from `main` between releases never claims to be the last release. The binary stays reproducible: the version is an input like the source. `runner-microvm version` prints it, and the startup `settings` line carries it (`docs/architecture.md`, Configuration).
- **`just release-assets <version>` builds what a release attaches, into `dist/`, which it empties first: the plugin as `runner-microvm-<version>-linux-amd64`, with the version set, and `SHA256SUMS` beside it in `sha256sum` format** (#124), so `sha256sum -c SHA256SUMS` in `dist/` checks it. A bare binary with a versioned name, no tarball: the release's source archives carry `LICENSE` (GPL-3.0's source offer), and a tarball would add only that. `dist/` and not `bin/`, so a release build never mixes with `bin/runner-microvm`, which says `dev`; it is ignored by git. `<version>` is passed as is, with no `v` and no validation: the maintainer runs it, and the release workflow (#125) passes the tag's version.
- **In a release, the job binary's dynamic loader is `/lib64/ld-linux-x86-64.so.2`, the x86_64 psABI's path; every other build keeps the dev shell's** (#124). The plugin and the agent are static, but the job binary is dynamically linked against glibc (`libc.so.6`, `libdl.so.2`, `libpthread.so.0`, no symbol versions), because purego `dlopen`s libkrun through the system's loader. Go writes its default loader path into the binary, and the dev shell's Go defaults to the Nix store's glibc, which no other host has: a release built with it can't run its job binary outside Nix, so the plugin stops at startup, when it first runs it to check the libraries. The standard path is where glibc distributions that follow the FHS, such as Debian, Ubuntu and Fedora, have their loader; a musl distribution doesn't have it. Dev and CI builds keep the Nix path, because the dev shell's libkrun is linked against the Nix store's glibc, and through the standard path the host's glibc, which may be older, would be loaded in its place. `release-assets` reads the job binary's interpreter back with `readelf` after the build and fails on any other, so a release can't ship one that names the wrong loader. (Flagged decision, #124: `release-assets` passes the path to `payload`, which links the job binary with `-I`. Rejected the standard path for every build, which breaks the tests on CI's Ubuntu for that reason, and patching the binary after the build, one more tool for one linker flag.)
- **One build path:** `just build` and `just release-assets` both call one private recipe, `plugin`, which builds the payload, then the plugin with the shared flags and `-tags payload`; only the output path, the linker flags and the job binary's loader differ. Two of the same `go build` line would drift.
- **No GoReleaser.** Its free version can't package a binary built elsewhere (`prebuilt` is Pro only), so it would be a second build definition; what it would add here is two shell lines. **Only the release workflow runs the recipe** (Releasing, below), and no PR or `main` run does: the maintainer decided the release path is proven at the first release, and fixed then if needed.

## The Nix package

```sh
nix develop -c just package       # nix build .#default: the flake's packages.default
nix develop -c just vendor-hash   # rewrite nix/package.nix's vendorHash for go.mod and go.sum
```

- **The flake's `packages.x86_64-linux.default` is `runner-microvm`, built by `nix/package.nix` with `buildGo126Module`, the dev shell's Go,** for NixOS hosts and the NixOS module (`docs/nixos.md`). Its version is the `VERSION` file's, so a package built from `main` between releases carries the last release's version, unlike `just build`'s `dev`: a flake has no tag to read, so `docs/nixos.md` tells users to pin a release tag.
- **Its build phase is `just plugin`, the recipe `build` and `release-assets` share,** with the version set and an empty loader, so the job binary keeps Go's default: the Nix store's glibc, the one the flake's libkrun is linked against (Building and the payload, above). The package's closure holds that glibc, because the embedded job binary names it. The derivation runs no tests: `just test` and `just test-kvm` do.
- **It carries the library directory it is meant for, `passthru.libDir`: the flake's libkrun and libkrunfw.** The package itself doesn't reference the libraries; the module's config file, whose built-in `lib_dir` names that directory, does.
- **`vendorHash` is the hash of the Go modules the build downloads, so it changes whenever `go.mod` or `go.sum` do, and only then.** Nix builds have no network, except fixed-output derivations, whose hash is declared in advance; Renovate can't run Nix to refresh it (`docs/conventions.md`, Renovate). `just vendor-hash` rewrites it with nix-update, from the dev shell: a commit that changes `go.mod` or `go.sum` runs it and includes the new hash. (Flagged decision: rejected committing `vendor/`, 46 MB, and a bot or self-hosted Renovate, which needs a write token.)
- **`just package` is part of `just check` and CI's `package` job,** so a stale `vendorHash` or a broken derivation fails the PR that causes it. It also builds the flake's `module` check: the NixOS module's unit, and `runner-microvm check` run on the config file the module writes (`docs/architecture.md`, The NixOS module's unit).

## Releasing

The maintainer releases in two steps, each one recipe, on a clean, up-to-date `main`:

```sh
nix develop -c just bump [version]        # 1. open the version PR; review and merge it like any other
nix develop -c just release               # 2. tag the chosen commit of main v<version> and push the tag
nix develop -c just release-notes         # the notes for the commits since the latest v* tag
```

- **Versioning and releasing are separate steps** (#125). The version is a reviewed commit on `main` before anything is tagged, and a release starts only from a tag, so nothing on `main`'s CI has to decide whether a push is a release.
- **The `VERSION` file at the repository root holds the bare version (`0.1.0`, no `v`).** It is what the version PR changes and what `just release` reads. The first version PR creates it. (Flagged decision, #125: rejected committing a placeholder such as `0.0.0` before then, a version that never existed and that `just release` would tag.)
- **SemVer tags `v<version>`, starting at `v0.1.0`, staying on 0.x while the runner's plugin protocol is `v1alpha`.** While the major version is 0, cocogitto never proposes 1.0.0: a breaking change bumps only as its type would (`feat` the minor, any other type the patch), so `just bump` with no version stays on 0.x. To bump the minor for a breaking `fix`, pass the version.
- **`just bump [version]` writes the given version, else what `cog bump --auto --dry-run` proposes from the Conventional Commits since the latest `v*` tag, to `VERSION`; commits `chore(release): v<version>` on a branch `release/v<version>`; pushes it; opens a PR with `gh` whose body is the notes preview (`just release-notes`); and switches back to the branch it started on.** So `just release` never runs by accident on the version branch, whose commit the rebase merge rewrites. A leading `v` on the version is dropped. Nothing is validated: the maintainer runs it. `gh` is in the dev shell for it, and uses the maintainer's own login.
- **The release notes are generated, never written by hand: `just release-notes [ref]` prints them for the commits up to `ref` (`HEAD` by default) since the `v*` tag before it, or from the first commit when there is none** (#125). The version PR's body is their preview, and the release workflow generates them again when it publishes, so commits merged after the version PR are listed too.
- **The notes start with two lines: "Tested with forgejo-runner <version>", the runner module's version in `go.mod`, which the protocol test runs against; and "Requires libkrun ≥ <min>, libkrunfw ≥ <min>", from `nix/lib-minimums.json`; both files as committed at `ref`.** So a past tag's notes show that tag's versions. One tested runner version and no range: nothing tests a range, and the plugin protocol (`v1alpha`) can change between runner releases.
- **Then `cog changelog`, showing only `feat`, `fix`, `perf`, `revert` and breaking changes** (`docs/conventions.md`, Commits and merging). `cog.toml` sets `tag_prefix = "v"`, so cocogitto reads the `v*` tags as versions. The two commits in `commits-ignore.txt` aren't Conventional Commits; `cog changelog` skips them with a warning.
- **`just release` tags `HEAD` as `v$(cat VERSION)`, an annotated tag, and pushes it** (#125). Whatever commit it runs on: versioning, merging more, then releasing is the maintainer's choice. Git refuses a tag that exists; nothing else is checked.
- **The release workflow (`.github/workflows/release.yml`) runs on a pushed `v*` tag. It calls `ci.yml` on the tagged commit, passing its secrets, and its `publish` job `needs:` that call, so it runs only once every CI job has passed** (`docs/conventions.md`, CI). That proves the tagged commit passed the same checks as every PR on that exact commit, trusting no earlier run, at the cost of running CI again.
- **`publish` runs `just release-assets <tag without v>`, attests the binary and `SHA256SUMS` with `actions/attest`, generates the notes with `just release-notes <tag>`, and creates the release in one `gh release create` with both files and the notes.** One call means no window where the release is visible without its binary. (Flagged decision, #125: `actions/attest` rather than the ticket's `actions/attest-build-provenance`, which since v4 is only a wrapper around it that its README tells new workflows not to use; both make the same SLSA build provenance.)
- **Before attesting, `publish` runs the job binary `release-assets` embedded, `internal/payload/runner-microvm-job version`, outside the dev shell, so through the Ubuntu runner's own glibc and its loader at `/lib64/ld-linux-x86-64.so.2`; the release stops if it can't run.** Every other test runs a job binary with the Nix store's loader, so without this nothing executes the one a release ships before operators do. It guards operators, not the maintainer, so it stands although the recipe itself has no CI job (Building and the payload, above). (Flagged decision, #125: rejected leaving `release-assets`'s `readelf` check alone, which proves the loader's path but not that the binary runs on a distribution's glibc, and running it in every PR's CI, which the maintainer ruled out for the recipe.)
- **A release carries the binary `runner-microvm-<version>-linux-amd64`, `SHA256SUMS`, a build provenance attestation and the generated notes,** besides GitHub's own source archives. The attestation is keyless, signed with a short-lived Sigstore certificate, and stored by GitHub; an operator checks it with `gh attestation verify`, requiring the release workflow as its signer and the release's tag as its source (`docs/operating.md`, Install the binary). No GPG or cosign keys to manage. (Flagged decision, #127: the guide's command is `--repo` with `--signer-workflow` naming `release.yml` and `--source-ref` naming the tag. Rejected the ticket's bare `--repo`, which accepts an attestation from any workflow of the repository on any branch or tag, and keeping it with a sentence claiming less, which leaves the operator a check that proves little.)
- **No SBOM file.** Go embeds the module list in the binary: `go version -m runner-microvm-<version>-linux-amd64` prints it.
- **No `CHANGELOG.md`.** It would be written at bump time and go stale when more commits land before the release; the version PR's preview and the release's notes are enough.
- **No release tool: no release-please, no GoReleaser.** The mechanics are plain glue. release-please adds a bot PR and a search for "a merged release PR not yet tagged" that races with other pushes to `main`; GoReleaser can't package a binary built elsewhere without Pro (Building and the payload, above).
- **x86_64 only, one binary; no release candidates, packages or service units.** Release candidates are a later addition; packaging is the consumer's (#12).
- **No tag rules or immutable releases are set up.** Repository settings are the maintainer's own.
- **`just release` and the release workflow are proven at the first release, `v0.1.0`; a fix that run needs is a normal PR.** The tag path can't run before a real tag, and throwaway tags or releases are ruled out.

## Running the plugin

```sh
nix develop -c just build
cat >/tmp/rm.yaml <<'EOF'
listen: unix:///tmp/rm/runtime/runner-microvm.sock
state_dir: /tmp/rm/state
cache_dir: /tmp/rm/cache
runtime_dir: /tmp/rm/runtime
EOF
RUNNER_MICROVM_CONFIG=/tmp/rm.yaml bin/runner-microvm check   # validates the file, prints every error
RUNNER_MICROVM_CONFIG=/tmp/rm.yaml bin/runner-microvm serve   # listens on /tmp/rm/runtime/runner-microvm.sock
```

- **`runner-microvm serve` runs the plugin; `runner-microvm check` validates the config file and exits; `runner-microvm version` prints the version (`dev` for every build but a release's) and exits.** None takes arguments. The config file is `/etc/runner-microvm/config.yaml`, or the file `RUNNER_MICROVM_CONFIG` names; its keys, built-in values and rules are in the reference, `docs/configuration.md`. An ordinary user can't write the built-in paths, so running the plugin by hand needs a config file that sets the four path keys, as above.
- **SIGINT or SIGTERM drains it: it takes no new jobs and exits once every job is removed. A second signal removes every job and exits at once** (`docs/architecture.md`, Stopping: the drain).
- **The host must have `mkfs.ext4` (e2fsprogs) on `PATH`.** The job process formats each VM's scratch disk with it (`docs/architecture.md`, The scratch disk). The plugin looks it up at startup and stops with an error naming it when it is missing, before it writes anything. The dev shell carries e2fsprogs, so the tests and the harness find it there. Formatting in the guest would need a mkfs the image may not have, or an ext4 writer of our own; an embedded template would need `resize2fs` for each profile's size anyway.
- **The cache directory's filesystem must support `renameat2`'s `RENAME_NOREPLACE`** (ext4, XFS, btrfs and tmpfs do). The image cache stores each layer with it, so a layer another Start stored first is never replaced under the VMs linking it (`docs/architecture.md`, Image cache). On a filesystem without it, each Start that stores a layer fails with an error naming the cache directory and the missing support.
- **The plugin addresses its unix sockets through an open directory, as `/proc/self/fd/<fd>/<name>`.** bind and connect take at most 107 bytes of path, which a checkout's `.e2e/plugin/runtime/` can outgrow on its own.

## Tests

```sh
nix develop -c just test       # go test -race ./...: no KVM, no root, no payload
nix develop -c just test-kvm   # go test -race -tags kvm,payload ./..., after just payload
```

- **`just test-kvm` runs every test, the `kvm`-tagged ones included,** against the embedded payload. It needs `/dev/kvm` and the dev shell's libraries, and runs as a step of CI's `kvm` job. It includes the protocol test (`docs/architecture.md`, Tests), which pulls `e2e/image` from Docker Hub and boots one VM.
- **The dependency test (`cmd/deps_test.go`) runs `go list -deps`,** so `go test` needs the `go` command on `PATH`, which `go test` provides.
- **forgejo-runner's main package is a `tool` in `go.mod`** (#127), so `go build code.forgejo.org/forgejo/runner/v13` builds the runner at the version the protocol test runs against, with its modules in `go.sum`. The runner-side example's test (`cmd/runnerconfig_test.go`) builds and runs it, because the runner's config loader is internal to its module and can't be imported. A tool is in the module graph only: no shipped binary, `just licenses` or `just vuln` sees it. (Flagged decision, #127: rejected `go run` of the runner at a version, which downloads outside `go.sum`, and copying the runner's loader, which would test a copy.)
- **It lists each binary as it ships, not as the test process is built:** `CGO_ENABLED=0` for all three, `-tags payload` for the plugin, `GOOS=linux GOARCH=amd64`, and an empty `GOFLAGS`. Under `-race` the test process has cgo on and no tags, and a file behind `//go:build !cgo` or `payload` is only in one of those graphs. The pinned values are constants in the test, so go test's cache keys on them. It passes `-e` so it works before `just payload`; an embed pattern matching no file is the only error it accepts.

## Protocols and codegen

The three protocols are `.proto` files under `proto/`, beside a copy of forgejo-runner's plugin protocol; `docs/architecture.md` (Protocols) says what each carries.

```sh
nix develop -c just generate          # regenerate the Go code from proto/
nix develop -c just check-generated   # fail if the committed code is stale
nix develop -c just lint              # payload, golangci-lint twice, then buf lint
```

- **`buf` with local plugins (`protoc-gen-go`, `protoc-gen-go-grpc`) from the dev shell, and no Buf Schema Registry.** One tool generates and lints, with no account or network. `buf.yaml` names the module (`proto/`) and the lint rules; `buf.gen.yaml` the plugins and output.
- **`buf lint` runs at its STANDARD rules, in `just lint`.** Each `.proto` file's directory mirrors its package, which has a version suffix (`runnermicrovm/agent/v1`).
- **forgejo-runner's plugin protocol is vendored as `proto/plugin/v1alpha/plugin.proto`, byte for byte as the runner ships it, and generated under `internal/gen/plugin/v1alpha/` like ours.** The runner's module is a test-only dependency, absent from every shipped binary (`cmd/deps_test.go`), so the plugin can't import the runner's own generated package; the file is MIT-licensed precisely so plugins can embed it. `buf lint` skips it, because its names are the runner's.
- **Go import paths come from buf's managed mode, so the `.proto` files carry no `go_package`.** The agent and job protocols are generated under `internal/gen/`; the VM child spec under `internal/job/internal/gen/`, so only the job binary can import it.
- **Generated code is committed,** so `go build` and `go install` need no codegen.
- **`just generate` deletes the generated directories and regenerates them,** so a removed `.proto` file leaves nothing behind.
- **`just check-generated` generates into a temporary directory and compares,** as part of `just check` and as CI's `generate` job. Stale generated code fails the PR, and the check works the same before and after `git add`.
- **The generated code needs Go modules at least as new as the generators:** `google.golang.org/protobuf` at least `protoc-gen-go`'s version, and a `google.golang.org/grpc` that supports `protoc-gen-go-grpc`'s output. A flake update that bumps a generator may need a module bump in the same PR.

## The local harness

The harness is a real Forgejo and a real forgejo-runner in one compose stack, with the plugin on the host, for development by the maintainer and agents. One command starts it, and another pushes a workflow and waits for it to go green, with no manual steps. A third reloads the plugin's config file. CI never runs it: CI's end-to-end coverage is the protocol test, which needs no Forgejo (#4).

```sh
nix develop -c just e2e up     # fresh state, the plugin, Forgejo and the runner; waits until the runner is online
nix develop -c just e2e run    # pushes e2e/workflows/echo.yml, dispatches it, prints the job's log and the VM's shape
nix develop -c just e2e run big   # the same for e2e/workflows/big.yml, or any workflow in e2e/workflows/
nix develop -c just e2e run clone # one of the Network exit-check workflows (The workflows, below)
nix develop -c just e2e run steps # steps' environment, user and working directory (The workflows, below)
nix develop -c just e2e run actions # a JavaScript action and a composite action, fetched by the runner (The workflows, below)
nix develop -c just e2e run disk  # one of the Mounts and disks exit-check workflows (The workflows, below)
nix develop -c just e2e reload # sends the plugin SIGHUP and prints the reload's log lines
nix develop -c just e2e down   # stops the plugin and every container, and deletes .e2e/
COMPOSE="podman compose" nix develop -c just e2e up   # the same, through podman compose
```

### Shape

- **One compose file, `e2e/compose.yaml`, running Forgejo (SQLite) and forgejo-runner.** That is the least that runs a real workflow.
- **No container registry.** The slice's test image is public, so nothing here needs one; the epic that first needs a private registry adds one.
- **It works with `docker compose` and `podman compose`, chosen by the `COMPOSE` variable, which defaults to `docker compose`.** The default talks to whatever Docker-API engine `DOCKER_HOST` names, so it runs on Docker or on a podman socket alike.
- **The dev shell carries everything the harness runs: the Docker CLI with its compose plugin, podman, `docker-compose` (which `podman compose` runs as its provider), curl, jq, git, and iproute2 for `ip`, which finds the host's address (below).** So `podman compose` needs nothing installed outside the shell. Podman comes from nixpkgs without the libkrun overlay: its runtime, crun, links libkrun, so the overlaid podman would miss the binary cache and rebuild libkrun.
- **What a dev shell cannot provide, the host must:**
  - for the default `docker compose`, a reachable Docker-API engine: a Docker daemon, or a podman socket that `DOCKER_HOST` points at;
  - for `podman compose`, a podman API service on podman's socket (`$XDG_RUNTIME_DIR/podman/podman.sock` when rootless): `podman compose` hands that socket to `docker-compose` as `DOCKER_HOST`, so the containers run in the service. A host's `podman.socket` unit serves it, or `podman system service --time=0` run from the shell;
  - for rootless podman, the setuid `newuidmap` and `newgidmap` and the user's subordinate ranges in `/etc/subuid` and `/etc/subgid`. Setuid binaries cannot live in the Nix store, and the ranges are system configuration;
- **Everything the stack writes is bind-mounted under `.e2e/` at the repo root, which is gitignored.** Every `up` starts by running `down`, so no run inherits state from the last one.
- **Each checkout gets its own compose project, `runner-microvm-e2e-` and the first 8 hex digits of the SHA-256 of the repo root's path; `COMPOSE_PROJECT_NAME` overrides it.** Several checkouts can share one host, and `up` starts with `down`: with one fixed name, a second checkout's `up` would silently destroy the first one's stack. With one project per checkout, `down` touches only its own containers and network, and a second checkout's `up` fails on the port, which only one stack can publish.
- **`down` empties `.e2e/` from a container, as the container's root, before deleting it.** The containers write as uids the host user may not own (rootless podman maps them to subordinate uids), so a plain `rm` on the host would fail.
- **The runner's container runs as `user: "0:0"`,** so it can open the plugin's socket, mode 0660 and the host user's. Under rootless podman, container root is the host user who owns the socket; under rootful Docker it is real root. As root, it also writes its registration in `.e2e/runner` with no world-writable directory.
- **The plugin runs on the host, not in the stack,** because it needs `/dev/kvm` and libkrun. `up` builds it (`just build`), writes its config file to `.e2e/plugin-config.yaml`, with its state, cache and runtime directories under `.e2e/plugin/`, the listen socket in the runtime directory, `lib_dir` naming the dev shell's libraries, and three profiles (The profiles, below), validates it with `check`, and starts `serve` in the background with `RUNNER_MICROVM_CONFIG` naming it, its pid in `.e2e/plugin.pid` and its log in `.e2e/plugin.log`: an agent can't easily keep a second terminal open. `down` stops it with SIGTERM, which removes every job and so every VM. A plugin still there after 30 seconds is killed the hard way: first each of its children's process groups, found in `/proc/<pid>/task/*/children` while it still runs, because each job process leads a group of its own, which killing the plugin's group would miss; then the plugin's group.
- **The runner's container mounts the plugin's runtime directory, `.e2e/plugin/runtime`, at `/run/runner-microvm`,** and its config points `plugins.runner-microvm.address` at the listen socket there.
- **Forgejo publishes `127.0.0.1:3000`, for the recipes and the developer, and the host's own address, port 3000, for the jobs.** A guest can't reach the host's loopback (NET-4), and its connections leave the host as the job process's, so they reach what the host publishes on its address. The host's loopback name server or `/etc/hosts` can't stand in: the guest resolves names through the host's resolver, and the host's own name commonly maps to a loopback address there.
- **That address is the source address of the host's IPv4 default route (`ip -4 route get`), unless `E2E_ADDRESS` names one.** Only `up` looks it up and checks it, first, before its `down`: it fails, naming `E2E_ADDRESS`, when there is no default route, or when the address is empty, not IPv4, loopback (`127.0.0.0/8`) or the wildcard (`0.0.0.0`). A job's guest can't reach loopback (NET-4) or connect to the wildcard, and `127.0.0.1` or the wildcard overlaps the `127.0.0.1:3000` binding, so Forgejo would fail to start. No other recipe needs the address, so `just check` and the rest work on a host with no route. `up` passes it to `e2e/compose.yaml`, which refuses to run without one; `down`, which publishes nothing, passes `192.0.2.1` (TEST-NET-1), so it works whatever the host's network. On a shared network this publishes the throwaway Forgejo to that network for as long as the stack runs; a developer who minds sets `E2E_ADDRESS` to an address only the host has.
- **Forgejo's `ROOT_URL` and the runner's instance URL are both `http://<that address>:3000`,** because the runner hands its instance URL to jobs: `github.server_url`, the artifact actions' `ACTIONS_RUNTIME_URL` and `ACTIONS_RESULTS_URL` all derive from it, and Forgejo builds the artifact actions' upload and download URLs from `ROOT_URL`. The compose service name `forgejo` resolves only inside the stack's network, which the guest isn't on. The runner's container reaches the host's address from the stack's bridge network; checked with rootless podman, whose pasta networking takes such a connection out to the host.

### Recipes

- **The recipes are a just module, `mod e2e`, loaded from `e2e/mod.just`:** `just e2e up`, `just e2e down`, `just e2e run [workflow]` and `just e2e reload`. It keeps them out of the root justfile's list and reads as sub-commands. Its recipes run in `e2e/`.
- **`up`** checks the host's address (above), runs `down`, builds and starts the plugin and waits for its socket, writes the runner's config and the secrets, starts Forgejo and waits until it is healthy, creates the admin, registers the runner, starts the runner, and waits until Forgejo's API shows it online (`idle` or `active`), for up to 120 seconds, then prints the compose project's name.
- **`run [workflow]`** runs `e2e/workflows/<workflow>.yml`, `echo` when none is named. It deletes and re-creates the test repository `e2e/<workflow>`, private, through Forgejo's API, pushes one commit to its `main` holding the workflow at `.forgejo/workflows/<workflow>.yml` and `e2e/actions/` at `actions/`, with git, since Forgejo's contents API can't set the executable bits the actions' scripts need, then triggers it with `workflow_dispatch`, polls the run for up to 300 seconds, prints each job's log, and exits 0 only when the run succeeded and a job log has the first step's output (`hello from runner-microvm e2e`) as a line of its own. Then it prints each `shape:` line the jobs logged. For `mount-write` alone, it deletes `.e2e/storage/rw/written` before the dispatch and, after the run, fails unless the job wrote it and it is owned by the user running the harness (The workflows, below). For `queue` alone, after the run it fails unless the plugin's log, from the dispatch on, has a `waiting for memory` and a `got memory` line, and prints them. A failing jobs list or log fetch fails it too, so a green `run` always shows the log. The expected text is the `expect` variable in `e2e/mod.just`, kept equal to the workflows' first step by hand: a change to one alone fails `run`. The repository is named after the workflow so that two runs of different workflows can be in flight at once, as the exit check below needs; each still starts from a fresh repository. It is private, so a job that reads it (`clone`, `checkout`) proves the token the runner gives the job works: anonymous reads get 401.
- **`reload`** sends the plugin SIGHUP, then waits up to 30 seconds for the plugin's log, from the signal on, to show the reload: it prints the reload's lines (each profile, any setting kept until a restart, and the summary) and exits 0, or prints the error and exits 1 when the reload failed. Editing `.e2e/plugin-config.yaml` first is the developer's step, as it is the operator's: `runner-microvm check` with `RUNNER_MICROVM_CONFIG=.e2e/plugin-config.yaml` tests an edit without reloading.
- **Both waits are wall-clock deadlines: 120 seconds for `up`, 300 for `run`.** Every curl has a timeout of its own, and a curl inside a wait never outlasts the time left, so neither wait can overrun however slow Forgejo answers.
- **`down`** stops this checkout's stack and its plugin, removes its network, and deletes `.e2e/`.

### Automatic setup, on every `up`

- **A random runner secret (40 hex digits) and admin password are generated into `.e2e/`.** They are throwaway, plain text and readable by the developer: `.e2e/admin-password` logs in as `e2e` at `http://127.0.0.1:3000`. They die at `down`, so nothing scrubs or hides them.
- **Forgejo's container creates the admin with `forgejo admin user create`.** No manual first-run setup.
- **The storage profile's fixtures are made under `.e2e/storage/`, each only when it is missing, before the plugin's config is checked, since `check` checks every source:** `ro/`, a directory holding `marker`, whose one line is the `ro_marker` variable in `e2e/mod.just`; `rw/`, an empty directory; and `disk.ext4`, a 64 MiB sparse file formatted with the dev shell's `mkfs.ext4 -q -F -E root_owner=0:0`, written to a temp name and renamed, so an interrupted `up` leaves no half-made disk. The harness owns its fixtures, as it does its other state: nothing is checked in, and `down` deletes them with `.e2e/`, so each `up` starts with an empty read-write mount and disk. The plugin runs on the host, so its config names them where they are, with nothing bound anywhere. (Flagged decision, #93: the ticket binds them into the plugin's container at the profile's paths, but the plugin has no container here; rejected moving the plugin into one, which needs `/dev/kvm` and libkrun inside it, for no gain.)
- **The runner is registered offline on both sides with the shared secret:** `forgejo forgejo-cli actions register --secret …` in Forgejo, then `forgejo-runner create-runner-file --secret …` as the runner's container starts. This needs no registration token and no API round trip. forgejo-runner v13 logs `create-runner-file` as deprecated in favour of declaring connections in its config; it still works, and moving off it is a change to `e2e/compose.yaml` alone.
- **The runner's config, `e2e/runner-config.yaml`, which `up` writes to `.e2e/runner-config.yaml` with `@IMAGE@` replaced by the image in `e2e/image`:**
  - four labels routed to the plugin: `microvm:runner-microvm://<the image in e2e/image>`, which names no profile, so its jobs get `default`; `microvm-big:runner-microvm://<the image in e2e/image>;profile=big`; `microvm-node:runner-microvm://<the image in e2e/node-image>`, with `@NODE_IMAGE@` replaced, which gets `default` too (The Node image, below); and `microvm-storage:runner-microvm://<the image in e2e/image>;profile=storage` (The profiles, below). The workflows don't name the backend, so `echo.yml` stayed the same when the label moved from the runner's `host` backend to the plugin;
  - `plugins.runner-microvm.address`, the plugin's listen socket as mounted in the runner's container;
  - `capacity: 2`, so two jobs reach the plugin at once and its memory budget, not the runner, decides which run together (The profiles, below). (Flagged decision, #121: it was 1, which runs one job at a time whatever the plugin does, so the Queueing epic's exit check could show no wait.)
  - the act cache server **off**, because nothing here needs a cache, and the guest can't reach a cache server on the host's loopback (NET-4).

### The workflows

- **`e2e/workflows/echo.yml`: one job, `runs-on: microvm`, no `container:`, a `run: echo …` step that exits 0, and a step that prints the VM's shape, triggered by `workflow_dispatch`.** It is the simplest real workflow; the protocol test checks the shape line against the built-in profile. A failing step and `actions/checkout` belong to #6. The protocol test reads this same file, as it reads every workflow here on `microvm` or `microvm-big`, so each workflow has one source.
- **`e2e/workflows/big.yml`: the same steps on `runs-on: microvm-big`,** so its VM has the `big` profile's shape.
- **`e2e/workflows/hold.yml`: `big.yml`'s steps, then `sleep ${{ inputs.seconds }}` and the shape again,** so a job is running while the config file is edited and reloaded. `seconds` is a `workflow_dispatch` input of default `60`, which the harness's dispatch, sending no inputs, takes, and which the protocol test overrides with `1` (`workflowInputs` in `cmd/protocol_test.go`), since nothing there reloads a running job's profile.
- **`e2e/workflows/steps.yml`: one job on `runs-on: microvm` whose steps check what a workflow author expects of a step (GST-4 to GST-7), each exiting non-zero otherwise, so the run's success is the check:** `env:` at job and step level, a step's `env:` staying in its step, `working-directory:` that exists and one, relative, that doesn't yet, a step output read by a later step, a variable written to `GITHUB_ENV` read by a later step, and `id` as the image's default user, root in `e2e/image`.
- **`e2e/workflows/actions.yml`: one job on `runs-on: microvm` in its own `container:` image, which uses a JavaScript action and a composite action, both fetched by the runner and copied into the VM (GST-10, GST-11).** Each action checks its own expectation and exits non-zero otherwise, and a last step checks both actions' outputs, so the run's success is the check:
  - **The actions are in `e2e/actions/`: `js`, run with the image's `node` (`using: node24`), checks its input, runs its own executable script, which works only if the copy kept its mode, and sets an output, a `GITHUB_ENV` variable and a state, which its post step checks; `composite`, whose one step, `shell: sh`, runs its own executable script, which checks its input and the JavaScript action's variable and sets the action's output.** The image has no bash, so the composite action's step says `shell: sh`; the runner falls back to `sh` for `run:` steps. The runner reads `GITHUB_OUTPUT`, `GITHUB_ENV` and `GITHUB_STATE` back with CopyOut.
  - **The job's own repository holds them at `actions/`, at the workflow's commit, and `uses:` names them from the `github` context: `${{ github.server_url }}/${{ github.repository }}/actions/js@${{ github.sha }}`.** The runner interpolates `uses:`, and the workflow schema allows the `github` context there. So one file serves both: in the harness the runner fetches the actions from Forgejo, with the job's token, since the repository is private and on the runner's own instance; in the protocol test, from its work directory served on loopback (`docs/architecture.md`, Tests), so CI needs no network for them. (Flagged decision, #56, recorded there.)
  - **Its image is `docker.io/library/node:24-alpine`, pinned by tag and digest in the workflow.** A JavaScript action needs `node` in the VM; naming the image in the workflow keeps `e2e/image` the small alpine every other test boots, and exercises the `container:` image end to end. It pulls under CI's Docker Hub login (The test image, below), and Renovate keeps its tag and digest current (`docs/conventions.md`, Renovate).
- **The Network epic's exit check (#7) is four workflows on `runs-on: microvm-node`, one per item, so a red run names what broke.** Each starts with the same `echo` step as the others, which `run` looks for.
  - **`clone.yml`: `git clone` of the job's own repository in a `run:` step, at `github.server_url` with the job's token (`github.token`) as the password, over HTTP inside the harness.** That is what a real workflow does, and it proves the guest reaches the Forgejo server through the gateway. The repository is private, so the clone fails without the token.
  - **`fetch.yml`: an HTTPS GET of `https://example.com/` with `curl`, after printing what `getent` resolves for it.** A stable public URL, so it proves DNS and TLS to the outside world.
  - **`checkout.yml`: `actions/checkout@v7`, then a check that the workflow's own file is in the workspace.** v7 is the newest major on `data.forgejo.org`, where Forgejo resolves `actions/…` by default; it runs on the image's `node`.
  - **`artifacts.yml`: an `upload` job that uploads a file with `https://data.forgejo.org/forgejo/upload-artifact@v4`, and a `download` job (`needs: upload`) that downloads it with `https://data.forgejo.org/forgejo/download-artifact@v4` and checks its content.** Forgejo implements its own artifact protocol, and its documentation for the pinned Forgejo, v17.0, says to use v3 or the patched v4 ([Advanced features, Artifacts](https://forgejo.org/docs/v17.0/user/actions/advanced-features/), whose v4 example uses these two actions). Upstream `actions/upload-artifact@v4` and later detect Forgejo as GHES and refuse to run.
  - **The actions are referenced by tag, not commit digest.** Renovate's GitHub Actions manager doesn't read `e2e/workflows/`, so a digest would go stale unnoticed; a tag follows its major, and the artifact actions must stay on v4 anyway. The harness never runs in CI.
- **The Mounts and disks epic's exit check (#9) is three workflows on `runs-on: microvm-storage`, one per item, so a red run names what broke.** Each starts with the same `echo` step as the others.
  - **`mount-read.yml`: reads `/mnt/ro/marker`, the storage profile's read-only mount, and checks it holds the fixture's text.**
  - **`disk.yml`: a `write` job that writes `run <github.run_id>` to `/mnt/disk/marker`, on the read-write disk, and a `read` job (`needs: write`) that reads it back and checks it names its own run.** Both run on the storage label, and `read`'s VM starts only after `write`'s is flushed and removed, so the run proves the flush across a real Remove (STO-9) and `read`'s Start never finds the disk in use: forgejo-runner sends a job's result to Forgejo only once the job's cleanup, the plugin's Remove included, has returned, so `needs:` can't schedule `read` earlier (v13.2.2: `Runner.Run` sends it from `reporter.Close`, deferred past the executor whose `cleanupJob` calls Remove, and periodic reports hold back a state with a result); naming the run means a second `run disk` in one `up` can't pass on the first one's marker.
  - **`mount-write.yml`: a step, as the guest's root (`id -u` is 0), writes `/mnt/rw/written` on the read-write mount; then `run` checks on the host that the file is there and owned by the uid of the user running the harness.** The plugin runs on the host as that user, unprivileged, so this is STO-3 for an unprivileged plugin with a root step (#89). Only the host can see the host-side owner, so the check is `run`'s, after the workflow.
  - **They run only in the harness, never in CI:** the protocol test skips them, naming `microvm-storage`, since it has no fixtures (`docs/architecture.md`, Tests).
- **The Job behaviour epic's exit check (#6) runs in the harness with these steps:**

  ```sh
  nix develop -c just e2e up
  nix develop -c just e2e run steps     # env:, working-directory:, step outputs and GITHUB_ENV
  nix develop -c just e2e run actions   # git clone of http://<address>:3000/e2e/actions, then "js: hello, microvm; composite: hi, microvm"
  nix develop -c just e2e down
  ```

- **The Network epic's exit check runs in the harness with these steps:**

  ```sh
  nix develop -c just e2e up
  nix develop -c just e2e run clone       # "Cloning into 'repo'...", then the repository's commit
  nix develop -c just e2e run fetch       # "fetched https://example.com/: HTTP 200"
  nix develop -c just e2e run checkout    # actions/checkout@v7 fetches from http://<address>:3000/e2e/checkout
  nix develop -c just e2e run artifacts   # the download job prints "artifact from run <id>"
  nix develop -c just e2e down
  ```

- **The Queueing epic's exit check (#10) is `queue.yml`: two parallel jobs, `first` and `second`, on `runs-on: microvm-big`, each printing the `echo` line, then sleeping 5 s.** `big`'s `memory_mib` is the harness's `max_memory_mib` (The profiles, below), and the runner takes both at once (`capacity: 2`), so one job's Start waits for the other's memory and boots once it is removed; the run must succeed with both jobs green, and `run` checks the plugin's log for the wait (Recipes, above). The protocol test runs it too, with no `max_memory_mib`, so there it shows only that both jobs pass.
- **The Queueing epic's exit check runs in the harness with these steps:**

  ```sh
  nix develop -c just e2e up
  nix develop -c just e2e run queue   # both jobs green; "run queue: ... msg="waiting for memory" ... need_mib=1536 free_mib=0 cap_mib=1536 ahead=0", then "... msg="got memory" ..."
  nix develop -c just e2e down
  ```

- **The Mounts and disks epic's exit check runs in the harness with these steps:**

  ```sh
  nix develop -c just e2e up            # makes .e2e/storage/ and the storage profile
  nix develop -c just e2e run mount-read    # "read from /mnt/ro/marker: read-only fixture of the runner-microvm e2e harness"
  nix develop -c just e2e run disk          # write: "wrote /mnt/disk/marker: run <id>"; read: "read from /mnt/disk/marker: run <id>"
  nix develop -c just e2e run mount-write   # ".e2e/storage/rw/written on the host: run <id>, owned by <you> (uid <yours>), the plugin's user"
  nix develop -c just e2e down
  ```

### The profiles

- **The plugin's config file in the harness defines `default` (1 CPU, 768 MiB, 2 GiB scratch), a larger profile, `big` (2 CPUs, 1536 MiB, 4 GiB scratch), and `storage`,** each key on a line of its own, so an edit for a reload is one `sed`. All three are small: the harness shares a developer's machine. **Its `max_memory_mib` is 1536, `big`'s memory,** so at most 1536 MiB of VMs run at once whatever the runner's capacity: two `big` jobs run one after the other, and two of the others at once. A `default` the file defines, unlike the protocol test's built-in one, shows that a label naming no profile gets the file's `default`.
- **`storage` is `default`'s shape with one read-only mount, one read-write mount and one read-write disk, and the runner's `microvm-storage` label names it:** `mounts` `.e2e/storage/ro` at `/mnt/ro`, `.e2e/storage/rw` at `/mnt/rw` with `read_write: true`, and `disks` `.e2e/storage/disk.ext4` at `/mnt/disk` with `read_write: true` (Automatic setup, above). It is the Mounts and disks epic's exit check's exact shape, and its three entries use all of amd64's 3 slots for mounts plus disks (CFG-10).
- **The Configuration and profiles epic's exit check (#8) runs in the harness with these steps:**

  ```sh
  nix develop -c just e2e up
  nix develop -c just e2e run         # shape: cpus=1, MemTotal near 768 MiB, 4194304 sectors (2 GiB): default
  nix develop -c just e2e run big     # shape: cpus=2, MemTotal near 1536 MiB, 8388608 sectors (4 GiB): big
  nix develop -c just e2e run hold > .e2e/hold.log 2>&1 &   # a big job that runs for a minute: hold's default
  until [ "$(grep -c msg=started .e2e/plugin.log)" -ge 3 ]; do sleep 1; done   # the third job, hold's, has started
  sed -i '/^  big:/,$ s/cpus: 2/cpus: 3/' .e2e/plugin-config.yaml
  nix develop -c just e2e reload      # profile name=big ... cpus=3, then "reloaded the config file"
  nix develop -c just e2e run big     # waits for the hold job's memory, then shape: cpus=3
  wait; grep '^run hold: shape' .e2e/hold.log   # both of the held job's shape lines: cpus=2
  nix develop -c just e2e down
  ```

  A label naming the larger profile sees its CPU count and memory, one naming no profile gets `default`, and after the edit and SIGHUP the next job gets the new values while the job running across the reload keeps its old ones (CFG-7). The plugin's log shows each job's profile at `created`. Both are `big`, whose memory is the whole memory budget, so the next job waits at Start for the held one's memory and boots once it ends; it was created after the reload, which is what decides its profile.

### The test image

- **`e2e/image` holds the one image the slice runs: `alpine:3.24`, pinned by tag and digest, as one line.** Both the harness's runner label and the protocol test read it. A `run:` step needs only a shell, and alpine's `sh` is enough; at about 4 MB, the naive pull costs seconds. Nothing is built.
- **It is pulled from Docker Hub on every run, with no cache step. In CI, the `kvm` job first logs in to Docker Hub** with `docker/login-action` and a read-only, public-repositories access token, from the repository secrets `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN`. The plugin reads that login from `~/.docker/config.json` through go-containerregistry's default keychain, so no code changes for it.
  - **Why log in:** anonymous pulls are limited by source address. Docker Hub allows 100 anonymous pulls per 6 hours per IPv4 address or IPv6 /64, and GitHub's hosted runners share their addresses, so other people's pulls count against that limit. ECR Public's anonymous copy (#46) failed the same way, with `TOOMANYREQUESTS: Data limit exceeded`. Logged in, pulls count against the account's own limit, 200 per 6 hours, far above what CI pulls.
  - **A fork's PRs get no secrets,** so the job skips the login there and pulls anonymously, under the shared limit.
  - Locally, and in the harness, the pull is anonymous: a developer's address isn't shared.
- **Renovate keeps its tag and digest current** (`docs/conventions.md`, Renovate).

### The Node image

- **The `microvm-node` label's image is in `e2e/node-image`: the official Node image on Debian, `node:26-trixie`, from ECR Public's mirror of Docker's official images (`public.ecr.aws/docker/library/node`), pinned by tag and digest, as one line.** The test image has neither `git` nor `node`, and actions need `node`; the full Debian variant carries `git` and `curl`, which the slim one lacks. 26 is Node's current LTS major. ECR Public avoids Docker Hub's anonymous pull limits (#46), and the digest keeps the harness reproducible. It is about 420 MiB compressed, pulled once per `up` into the plugin's image cache.
- **Only the harness reads it;** the protocol test and the other tests keep `e2e/image`, or, for `actions.yml`, the workflow's own `container:` image.
- **Renovate keeps its tag and digest current,** through the same regex manager as `e2e/image` (`docs/conventions.md`, Renovate). `config:recommended` applies Renovate's Node versioning to it, so it proposes LTS majors only.

### Versions

- **Forgejo and the runner images are pinned by tag and digest in `e2e/compose.yaml`:** Forgejo 17.0.2, the latest stable release, and forgejo-runner v13.2.2. v13.1.0 is the first release with the plugin protocol. Both come from `data.forgejo.org`, Forgejo's own registry.
- **Renovate updates them** (`docs/conventions.md`, Renovate).

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

- **The dev shell exports `RUNNER_MICROVM_LIBDIR`:** the one directory holding both libraries. Only the tests, the harness and `just check-libs` read it; the plugin reads no variable but `RUNNER_MICROVM_CONFIG`, so the tests and the harness write the directory into the plugin's config file as `lib_dir` (#95).
- **`just check-libs` checks that directory.** `libkrun.so.1` and `libkrunfw.so.5` must resolve to real files whose versioned names are at least the minimums. It prints both versions.

### KVM in CI

- **The `kvm` job on `ubuntu-24.04` proves a GitHub x86 runner can use `/dev/kvm`.** It does five things:
  1. Adds a udev rule granting `/dev/kvm` read-write to everyone (`KERNEL=="kvm", GROUP="kvm", MODE="0666"`).
  2. Reloads udev.
  3. Opens `/dev/kvm` read-write as the runner user.
  4. Runs `nix develop -c just check-libs`, then `nix flake check`, so a library below its minimum also fails CI.
  5. Runs `nix develop -c just test-kvm -v`.
- **The job then runs `nix develop -c just test-kvm -v`:** the tier for tests that boot VMs (`kvm` build tag), the protocol test among them. It runs as the ordinary runner user, which proves the plugin needs nothing but `/dev/kvm` (VM-9); the tests that run the plugin as root reach root through the job's passwordless `sudo -n` (`docs/architecture.md`, Running as root).
- **`-v` makes the log list every test's `PASS` or `SKIP`, so it shows that the root cases ran rather than skipped** (#96). A root case that can't get root fails rather than skips with `CI` set, which GitHub sets, so CI can't pass without them; the list makes that visible without reading the test. `just test-kvm` passes any arguments on to `go test`, and run locally it takes none. A passing test logs neither its job logs nor its plugin's log, so the list stays readable; a failing one logs both in full (`docs/architecture.md`, Tests). (Flagged decision, #96: rejected a second step that reruns only the root cases verbosely, which runs them twice, and `go test -json` with a summary, which loses the streamed log.)
- **It uses the same Nix install and store cache as the other jobs.** The cache matters here, because the libkrunfw overlay builds a kernel. The cache key includes `nix/`, so a change to the overlays builds and saves a fresh store.
- **`setup-nix` roots the dev shell with `nix develop --profile`,** because `nix develop` makes no GC root and the cache action collects garbage before it saves, which would delete the libraries and make the next run rebuild them.
