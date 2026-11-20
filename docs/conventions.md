# Conventions

The project's definition of done, and its rules for commits, pull requests, source files, lint, tests, package boundaries, dependencies and dependency updates, each with its reason. `nix develop -c just check` runs every check below; CI runs each as its own job.

## Definition of done

A change is done when:

- `nix develop -c just check` passes;
- the issue's acceptance checks are shown passing in the PR;
- `docs/` and `GLOSSARY.md` are updated in the same PR;
- there are no unmarked shortcuts: every deliberately naive piece of code carries `// SHORTCUT(#<issue>): <what is naive>`, naming the issue that replaces it.

`AGENTS.md` stays a lean index, so the conventions live here.

## Commits and merging

- **Every commit is a [Conventional Commit](https://www.conventionalcommits.org/).** It gives the release epic (#12) a history it can build a changelog from.
- **PRs are rebase-merged,** so each logical commit lands on `main` as written. That keeps room for PRs with several meaningful commits.
- **A PR's commits are its logical changes only; a fix to something the PR already contains is folded into the commit that introduced it,** never added as a new commit. That covers review findings, red CI and follow-ups: `git commit --fixup=<commit>`, then `git rebase --autosquash` locally, then force-push the PR branch. Rebase merge lands every commit on `main` as written, so a follow-up commit would become history.
- **cocogitto checks the messages** (`cog check`, configured in `cog.toml`). It is a single binary from the flake, with no Node toolchain.
  - `just lint-commits` checks `origin/main..HEAD` and is part of `just check`. `just lint-commits <range>` checks another range.
  - CI's `commits` job checks the PR's own commits (its base to its head), or the commits a push to `main` added.
  - Two commits on `main` predate the rule and are not Conventional Commits: the initial empty commit and the agent-skills setup. `commits-ignore.txt` lists them, and the commit check skips them. The changelog (#12) must skip them too. No later commit is ever added to the list; its message is fixed before it merges.
  - `cog check` can't skip commits, and cocogitto 7.0.0 checks the whole history when a range starts at the root commit. When a range contains an ignored commit or starts at the root, the recipe runs `cog verify` on each commit itself.
- **Rules:**
  - The allowed types are `feat`, `fix`, `docs`, `refactor`, `test`, `build`, `ci`, `chore`, `perf` and `revert`: the standard set. cocogitto's extra default, `style`, is turned off.
  - Scopes are optional and free-form.
  - A breaking change is marked with `!` or a `BREAKING CHANGE:` footer.
  - Merge commits fail, because they can't land on `main` under rebase merging.

## Showing a check going red

- **Never open a throwaway pull request.** To show a check failing, push the breaking commit to the PR you intend to merge, record the red run's link, then force-push the PR back to its good state. Throwaway PRs and branches are clutter for the maintainer.

## PR template

`.github/pull_request_template.md` has three sections:

- **Summary.**
- **`Closes #<n>`.**
- **Exit check:** the command or check a reviewer can watch pass.

It ends with a checklist: `just check` is green, and durable decisions are recorded in `docs/`. The exit check makes every PR state how to verify it, and the checklist catches the two things reviewers most often find missing.

## SPDX headers

- **Every source file starts with `SPDX-License-Identifier: GPL-3.0-or-later`,** in that language's comment form. One line states the license where the code is read.
- Here "source file" means Go, Protocol Buffers (`.proto`), Nix, YAML, TOML, the `justfile`, `go.mod`, `renovate.json5`, `.envrc` and `.gitignore`. Markdown, `LICENSE`, and generated files without comments (`flake.lock`, `go.sum`) are exempt.
- The generated Go code carries the header because the generators copy it from the `.proto` file's leading comment.
- **goheader enforces the header for Go** as part of `just lint`, so no extra tool is needed.
- **No REUSE tooling:** one header line is enough for this repo.

## Lint

golangci-lint v2 from the dev shell, configured in `.golangci.yml`. `just lint` checks; `just fmt` applies the formatters.

- **Strict, no warnings: any finding fails the build.** Warnings get ignored, especially by agents.
- **The base set is `standard`** (errcheck, govet, ineffassign, staticcheck, unused).
- **Added linters:** errorlint, gocritic, revive, misspell, unconvert, unparam, bodyclose, contextcheck, noctx, errname, exhaustive, gosec, copyloopvar, usestdlibvars, nolintlint and goheader. They catch real bugs (error wrapping, contexts, leaked bodies, missing switch cases, security smells) rather than matters of taste.
- **nolintlint requires a specific linter name and an explanation on every `nolint`,** so a skipped rule is always one line with a reason.
- **The formatters are gofumpt and goimports,** run through golangci-lint's formatter support. gofumpt is a strict superset of gofmt, which removes formatting debates.
- **Test files follow the same rules, except gosec and unparam.** Fixtures trip gosec, and test helpers often take constant arguments.
- **Generated code (`internal/gen/`, `internal/job/internal/gen/`) is excluded from linting and formatting.** It is not ours to change, and `just check-generated` keeps it in step with `proto/`.
- **`just lint` also runs `buf lint` at its STANDARD rules** on `proto/` (see `docs/development.md`).
- **No caps on reported issues** (`max-issues-per-linter` and `max-same-issues` are 0), so nothing is hidden.

## Tests

- **`just test` runs `go test -race ./...`.** The race detector catches concurrency bugs early, and the plugin is concurrent by design. It needs no KVM, no root and no payload.
- **Tests that boot VMs carry `//go:build kvm`.** `just test-kvm` runs every test with `-tags kvm,payload`, as a step of CI's `kvm` job (see `docs/architecture.md`, Tests).

## Boundary enforcement

- **The package boundaries in `docs/architecture.md` are enforced, not just documented.** The compiler keeps `internal/job/internal/` private to the job process, and `cmd/deps_test.go` runs `go list -deps` on each binary under plain `go test ./...`: the plugin must not depend on purego, gvisor, gvisor-tap-vsock, `internal/job` or `internal/agent`; the job binary must depend on purego and gvisor-tap-vsock; the agent may depend only on its allow-list.
- **The test checks transitive dependencies,** which is the property that matters. depguard checks only direct imports, so it isn't used.
- **A change that needs a boundary moved changes `docs/architecture.md` and the test in the same PR.**

## Dependency licenses

`just licenses` runs go-licenses v2 against the allow-list in [ADR 0001](adr/0001-gpl-3-or-later-and-dependency-allow-list.md).

- **The allow-list, as go-licenses spells it:** `MIT`, `BSD-2-Clause`, `BSD-3-Clause`, `Apache-2.0`, `MPL-2.0`, `LGPL-2.1`, `LGPL-3.0`, `GPL-3.0`, `ISC`, `0BSD`, `Unlicense` and `CC0-1.0`. The ADR's "BSD" means the two GPL-compatible BSD licenses; BSD-4-Clause is not GPL-compatible.
- **Only the Go modules are checked.** The reasons:
  - The flake's inputs are dev tools and are never distributed.
  - libkrun is installed by the operator.
  - The embedded guest agent is Go, so the check covers it.
- **A module whose license can't be detected fails the check.**
- **Reviewed exceptions go in `licenses-ignore.txt`,** one package path per line. Each entry has a comment above it saying what was checked.

## Vulnerabilities

- **`just vuln` runs govulncheck, as its own CI job, and a finding blocks the PR.** govulncheck reports only vulnerabilities in code the binary actually reaches, so a failure is worth acting on.

## CI

- **GitHub Actions on `ubuntu-24.04`,** triggered by every pull request, whatever its base, and by pushes to `main`. Running on PRs with any base gives stacked PRs checks too.
- **Nix comes from `cachix/install-nix-action`.** The Nix store is cached with `nix-community/cache-nix-action`, which uses the Actions cache and needs no account or secret. Both steps live in `.github/actions/setup-nix`.
  - The cache key is the hash of `flake.nix` and `flake.lock`.
  - Pushes to `main` purge older entries once a new one is saved, so stale stores don't crowd out the repo's cache quota. PR runs never purge, so a PR that changes the flake can't delete `main`'s entry.
- **A new push to a PR cancels its older run; runs on `main` are never cancelled,** so every push to `main` is checked and saves its cache.
- **The jobs are `lint`, `generate`, `test`, `licenses`, `vuln` and `commits`.** Each runs `nix develop -c just <recipe>`. Separate jobs show which rule failed at a glance.
- **Every Action is pinned to a commit digest, with its version as a comment.** Renovate keeps the digests current.
- **The `kvm` job also runs `just test-kvm`,** the only tier that boots VMs (see `docs/development.md`, KVM in CI).
- **CI never runs Forgejo.** End-to-end runs use the local harness (`docs/development.md`, The local harness).

## Renovate

`renovate.json5` configures it; a comment above each rule names the decision it implements.

- **After editing `renovate.json5`, validate it by hand: `nix shell nixpkgs#renovate -c renovate-config-validator --strict`, run at the repo root.** With no argument it finds the file and checks it as repo config; given the path, it needs `--no-global`, or it checks it as self-hosted global config. The dev shell, the justfile and CI don't run it: the file changes rarely, and the validator is a 250 MiB package.
- **Renovate runs as the Mend-hosted GitHub App:** no runner or token to maintain.

### What it updates

- **Go modules, with `go mod tidy` after every update and import paths rewritten on major bumps** (`gomodTidy`, `gomodUpdateImportPaths`). The runner's plugin protocol is imported from `code.forgejo.org/forgejo/runner/v13/...`, and a major bump changes that path.
- **Not the `go` directive in `go.mod`:** a Go minor bump is a deliberate change of `go.mod` and the flake together, and patch releases arrive through `flake.lock` (see `docs/development.md`).
- **GitHub Actions, pinned to commit digests with the version as a comment** (`helpers:pinGitHubActionDigests`). A digest can't be moved under us the way a tag can.
- **Flake inputs, through weekly `flake.lock` maintenance only,** never as a PR per input. This is how Go patch releases and tool updates arrive. The `nix` manager is off by default in Renovate, so the config enables it.
- **The harness's images in `e2e/compose.yaml`, Forgejo and forgejo-runner, pinned by tag and digest.** `config:recommended` enables the `docker-compose` manager, which updates the tag and the digest together.
- **Not the libkrun and libkrunfw overlays:** a version bump also needs a hash change Renovate can't make (#15). No Renovate manager reads `nix/libkrun-overlay.nix` or `nix/lib-minimums.json`, and the config adds no custom manager that would.

### Grouping and schedule

- **Weekly, early on Monday (UTC).** Fewer, larger PRs keep review load low.
- **One group for non-major Go module updates, one for non-major Actions updates; `flake.lock` maintenance is a PR of its own.** Each major update gets a PR of its own, so it can't hold back a group that automerges.
- **`code.forgejo.org/forgejo/runner` updates get a group of their own,** so a runner update waiting for review doesn't hold back the automerging Go group. **The harness's runner image (`data.forgejo.org/forgejo/runner`) joins that group,** so when updates to the runner's Go module and its image are pending together, they arrive in one PR. Grouping doesn't make one wait for the other: a release of only one of them, or a re-pushed image digest with no new tag, opens the group's PR with that update alone.

### Automerge

- **Non-major Go module updates, Action digest and version updates, and `flake.lock` maintenance automerge once every required check passes.** CI is the gate, and these are low risk.
- **Never automerged: any major update, and any update to `code.forgejo.org/forgejo/runner` or its image,** whose plugin protocol is the plugin's contract with the runner. They wait for the maintainer.
- **The harness's Forgejo image is not automerged either:** no rule names it, so its updates get Renovate's default, a PR of their own that waits for the maintainer. The harness is local only, so no required check would exercise the new image.
- **Automerge is GitHub's platform automerge, gated by `main`'s required checks, and uses the repo's rebase merge** (`automergeStrategy: rebase`), like every other PR.

### Commit messages

- **Renovate's commits are Conventional Commits,** so they pass the commit check: `build(deps): …` for Go modules and flake inputs, `ci(deps): …` for Actions. The config overrides `config:recommended`, which would make Go modules `fix(deps)`.
