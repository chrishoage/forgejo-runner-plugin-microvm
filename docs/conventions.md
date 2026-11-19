# Conventions

The project's rules for commits, pull requests, source files, lint, dependencies and dependency updates, each with its reason. `nix develop -c just check` runs every check below; CI runs each as its own job.

## Commits and merging

- **Every commit is a [Conventional Commit](https://www.conventionalcommits.org/).** It gives the release epic (#12) a history it can build a changelog from.
- **PRs are rebase-merged,** so each logical commit lands on `main` as written. That keeps room for PRs with several meaningful commits.
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

## PR template

`.github/pull_request_template.md` has three sections:

- **Summary.**
- **`Closes #<n>`.**
- **Exit check:** the command or check a reviewer can watch pass.

It ends with a checklist: `just check` is green, and durable decisions are recorded in `docs/`. The exit check makes every PR state how to verify it, and the checklist catches the two things reviewers most often find missing.

## SPDX headers

- **Every source file starts with `SPDX-License-Identifier: GPL-3.0-or-later`,** in that language's comment form. One line states the license where the code is read.
- Here "source file" means Go, Nix, YAML, TOML, the `justfile`, `go.mod`, `.envrc` and `.gitignore`. Markdown, `LICENSE`, and generated files without comments (`flake.lock`, `go.sum`) are exempt.
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
- **No caps on reported issues** (`max-issues-per-linter` and `max-same-issues` are 0), so nothing is hidden.

## Tests

- **`just test` runs `go test -race ./...`.** The race detector catches concurrency bugs early, and the plugin is concurrent by design.

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
- **The jobs are `lint`, `test`, `licenses`, `vuln` and `commits`.** Each runs `nix develop -c just <recipe>`. Separate jobs show which rule failed at a glance.
- **Every Action is pinned to a commit digest, with its version as a comment.** Renovate keeps the digests current.
- **CI never runs Forgejo.** End-to-end runs use a local harness (#4).

## Renovate
