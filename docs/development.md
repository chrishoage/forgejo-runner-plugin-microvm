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
- **x86_64 Linux only:** the flake defines the dev shell for `x86_64-linux` alone, because GitHub's x86 Linux runners provide KVM; arm64 is out of scope.
- **The flake selects `go_1_26` explicitly,** so a change to nixpkgs' default `go` can't silently change the Go minor version. The shell's Go tools (golangci-lint, go-licenses, govulncheck) are built with that same Go, so they agree with `go` on the standard library; nixpkgs would otherwise build them with its latest Go.
- **`go.mod` states the language minimum `go 1.26` (written `1.26.0` once a dependency requires that form), with no `toolchain` line, and the dev shell sets `GOTOOLCHAIN=local`,** so Go never downloads a toolchain of its own: the flake is the only source of Go.
- **Go patch releases arrive through `flake.lock` updates.** A minor bump (1.27) is a deliberate change of `go.mod` and the flake together.
- **The dev shell carries every tool the project's checks need:** Go, just, golangci-lint, go-licenses, govulncheck, cocogitto, and buf with protoc-gen-go and protoc-gen-go-grpc. It also carries a C compiler, because `go test -race` needs cgo even though the binary builds without it (`CGO_ENABLED=0`).
- **`.envrc` contains `use flake`,** so direnv users get the shell on `cd`.
- **`just` is the task runner:** every routine command is a recipe, so the maintainer, agents and CI run the same commands.

## Building and the payload

`runner-microvm` embeds the job binary and the agent (`docs/architecture.md`, The embedded payload).

```sh
nix develop -c just payload   # internal/payload/runner-microvm-{job,agent}
nix develop -c just build     # runs payload, then bin/runner-microvm with -tags payload
```

- **`just build` always builds the payload first,** then the plugin with `-tags payload`. Every binary builds with `CGO_ENABLED=0 -trimpath -buildvcs=false`, so the plugin binary is reproducible under Nix.
- **`internal/payload/embedded.go` (tag `payload`) embeds the two files by exact name,** so a build with the tag but without the payload fails to compile. Without the tag, `internal/payload/stub.go` returns `ErrNoPayload`, so `go build ./...`, `go vet` and `go test` need no payload.
- **`just lint` builds the payload, then lints twice:** once as `go test` builds (cgo on, no tags), and once as the binaries ship (`CGO_ENABLED=0`, `-tags payload`). One pass leaves the other's files unlinted: `embedded.go` hid an unused variable that way. A second pass costs about a second, and the payload build is cached after `just build`; golangci-lint's `build-tags` setting would replace one view instead of adding the other.
- **The built payload is ignored by git and never committed.**

## Tests

```sh
nix develop -c just test       # go test -race ./...: no KVM, no root, no payload
nix develop -c just test-kvm   # go test -race -tags kvm,payload ./..., after just payload
```

- **`just test-kvm` runs every test, the `kvm`-tagged ones included,** against the embedded payload. It needs `/dev/kvm` and the dev shell's libraries, and runs as a step of CI's `kvm` job.
- **The dependency test (`cmd/deps_test.go`) runs `go list -deps`,** so `go test` needs the `go` command on `PATH`, which `go test` provides.
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

The harness is a real Forgejo and a real forgejo-runner in one compose stack, for development by the maintainer and agents. One command starts it, and another pushes a workflow and waits for it to go green, with no manual steps. CI never runs it: CI's end-to-end coverage is the protocol test, which needs no Forgejo (#4).

```sh
nix develop -c just e2e up     # fresh state, Forgejo and the runner; waits until the runner is online
nix develop -c just e2e run    # pushes e2e/workflows/echo.yml, dispatches it, prints the job's log
nix develop -c just e2e down   # stops every container and deletes .e2e/
COMPOSE="podman compose" nix develop -c just e2e up   # the same, through podman compose
```

### Shape

- **One compose file, `e2e/compose.yaml`, running Forgejo (SQLite) and forgejo-runner.** That is the least that runs a real workflow.
- **No container registry.** The slice's test image is public, so nothing here needs one; the epic that first needs a private registry adds one.
- **It works with `docker compose` and `podman compose`, chosen by the `COMPOSE` variable, which defaults to `docker compose`.** The default talks to whatever Docker-API engine `DOCKER_HOST` names, so it runs on Docker or on a podman socket alike.
- **The dev shell carries everything the harness runs: the Docker CLI with its compose plugin, podman, `docker-compose` (which `podman compose` runs as its provider), curl and jq.** So `podman compose` needs nothing installed outside the shell. Podman comes from nixpkgs without the libkrun overlay: its runtime, crun, links libkrun, so the overlaid podman would miss the binary cache and rebuild libkrun.
- **What a dev shell cannot provide, the host must:**
  - for the default `docker compose`, a reachable Docker-API engine: a Docker daemon, or a podman socket that `DOCKER_HOST` points at;
  - for `podman compose`, a podman API service on podman's socket (`$XDG_RUNTIME_DIR/podman/podman.sock` when rootless): `podman compose` hands that socket to `docker-compose` as `DOCKER_HOST`, so the containers run in the service. A host's `podman.socket` unit serves it, or `podman system service --time=0` run from the shell;
  - for rootless podman, the setuid `newuidmap` and `newgidmap` and the user's subordinate ranges in `/etc/subuid` and `/etc/subgid`. Setuid binaries cannot live in the Nix store, and the ranges are system configuration;
- **Everything the stack writes is bind-mounted under `.e2e/` at the repo root, which is gitignored.** Every `up` starts by running `down`, so no run inherits state from the last one.
- **Each checkout gets its own compose project, `runner-microvm-e2e-` and the first 8 hex digits of the SHA-256 of the repo root's path; `COMPOSE_PROJECT_NAME` overrides it.** Several checkouts can share one host, and `up` starts with `down`: with one fixed name, a second checkout's `up` would silently destroy the first one's stack. With one project per checkout, `down` touches only its own containers and network, and a second checkout's `up` fails on the port, which only one stack can publish.
- **`down` empties `.e2e/` from a container, as the container's root, before deleting it.** The containers write as uids the host user may not own (rootless podman maps them to subordinate uids), so a plain `rm` on the host would fail.
- **The runner's state directory, `.e2e/runner`, is world-writable.** The runner image runs as uid 1000, which must write its registration there whatever the host's uid or user namespace. The runner running as container root, for the plugin's socket, comes with #33.
- **Forgejo publishes only `127.0.0.1:3000`, for the API.** Nothing else needs to reach it from outside the stack.

### Recipes

- **The recipes are a just module, `mod e2e`, loaded from `e2e/mod.just`:** `just e2e up`, `just e2e down` and `just e2e run`. It keeps them out of the root justfile's list and reads as sub-commands. Its recipes run in `e2e/`.
- **`up`** runs `down`, writes the secrets, starts Forgejo and waits until it is healthy, creates the admin, registers the runner, starts the runner, and waits until Forgejo's API shows it online (`idle` or `active`), for up to 120 seconds, then prints the compose project's name.
- **`run`** deletes and re-creates the test repository `e2e/echo` through Forgejo's API, commits the workflow to `.forgejo/workflows/echo.yml`, triggers it with `workflow_dispatch`, polls the run for up to 300 seconds, prints each job's log, and exits 0 only when the run succeeded and a job log has the step's output (`hello from runner-microvm e2e`) as a line of its own. A failing jobs list or log fetch fails it too, so a green `run` always shows the log. The expected text is the `expect` variable in `e2e/mod.just`, kept equal to the workflow's step by hand: a change to one alone fails `run`.
- **Both waits are wall-clock deadlines: 120 seconds for `up`, 300 for `run`.** Every curl has a timeout of its own, and a curl inside a wait never outlasts the time left, so neither wait can overrun however slow Forgejo answers.
- **`down`** stops this checkout's stack, removes its network, and deletes `.e2e/`.

### Automatic setup, on every `up`

- **A random runner secret (40 hex digits) and admin password are generated into `.e2e/`.** They are throwaway, plain text and readable by the developer: `.e2e/admin-password` logs in as `e2e` at `http://127.0.0.1:3000`. They die at `down`, so nothing scrubs or hides them.
- **Forgejo's container creates the admin with `forgejo admin user create`.** No manual first-run setup.
- **The runner is registered offline on both sides with the shared secret:** `forgejo forgejo-cli actions register --secret …` in Forgejo, then `forgejo-runner create-runner-file --secret …` as the runner's container starts. This needs no registration token and no API round trip. forgejo-runner v13 logs `create-runner-file` as deprecated in favour of declaring connections in its config; it still works, and moving off it is a change to `e2e/compose.yaml` alone.
- **The runner's config, `e2e/runner-config.yaml`:**
  - one label, `microvm:host`: the runner's built-in `host` backend, so the harness proves itself before the plugin exists. #33 maps the label to the plugin instead, and the workflow stays the same;
  - `capacity: 1`;
  - the act cache server **off**, because the slice's guest has no network until #7, and nothing here needs a cache.

### The workflow

- **`e2e/workflows/echo.yml`: one job, `runs-on: microvm`, no `container:`, one `run: echo …` step that exits 0, triggered by `workflow_dispatch`.** It is the simplest real workflow. A failing step and `actions/checkout` belong to #6. The slice's protocol test reads this same file, so the workflow has one source.

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

- **The dev shell exports `RUNNER_MICROVM_LIBDIR`:** the one directory holding both libraries, which the plugin will later read to find them (#1).
- **`just check-libs` checks that directory.** `libkrun.so.1` and `libkrunfw.so.5` must resolve to real files whose versioned names are at least the minimums. It prints both versions.

### KVM in CI

- **The `kvm` job on `ubuntu-24.04` proves a GitHub x86 runner can use `/dev/kvm`.** It does five things:
  1. Adds a udev rule granting `/dev/kvm` read-write to everyone (`KERNEL=="kvm", GROUP="kvm", MODE="0666"`).
  2. Reloads udev.
  3. Opens `/dev/kvm` read-write as the runner user.
  4. Runs `nix develop -c just check-libs`, then `nix flake check`, so a library below its minimum also fails CI.
  5. Runs `nix develop -c just test-kvm`.
- **The job then runs `nix develop -c just test-kvm`:** the tier for tests that boot VMs (`kvm` build tag). Until #4 adds such tests, it runs the plain tests against the embedded payload.
- **It uses the same Nix install and store cache as the other jobs.** The cache matters here, because the libkrunfw overlay builds a kernel. The cache key includes `nix/`, so a change to the overlays builds and saves a fresh store.
- **`setup-nix` roots the dev shell with `nix develop --profile`,** because `nix develop` makes no GC root and the cache action collects garbage before it saves, which would delete the libraries and make the next run rebuild them.
