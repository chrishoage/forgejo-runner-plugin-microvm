# AGENTS.md

## Rules

- Run everything through the dev shell and `just` (`nix develop -c just <recipe>`). `just check` must pass before a PR.
- Every commit is a Conventional Commit, and lands on `main` as written (rebase merge).
- No `nolint` without a reason on that line.

## Docs

Durable decisions live here, not in this file or in issues.

- `docs/development.md`: how the project is built, the toolchain and the dev shell.
- `docs/conventions.md`: commits and merging, the PR template, SPDX headers, lint, dependency licenses, Renovate.
- `docs/adr/`: decisions that are hard to reverse.
- `docs/agents/`: how agent skills use the issue tracker, triage labels and domain docs.

## Agent skills

### Issue tracker

Issues are tracked in GitHub Issues for chrishoage/forgejo-runner-plugin-microvm, via the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

Default vocabulary: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `GLOSSARY.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`.
