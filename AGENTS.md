# AGENTS.md

## Rules

- Run everything through the dev shell and `just` (`nix develop -c just <recipe>`). `just check` must pass before a PR.
- Every commit is a Conventional Commit, and lands on `main` as written (rebase merge).
- A PR's commits are its logical changes only. Fold a fix to something the PR contains (review findings, red CI, follow-ups) into the commit that introduced it (`git commit --fixup`, `git rebase --autosquash` locally, force-push the PR branch); never add it as a new commit.
- No `nolint` without a reason on that line.
- Never open throwaway pull requests. To show a check going red, push the breaking commit to the PR you intend to merge, record the run, then force-push it away.
- The spike is a reference for behaviour only: never copy its code, layout or patterns.
- A deliberate shortcut carries `// SHORTCUT(#<issue>): <what is naive>`. `git grep SHORTCUT` finds them all, and the issue that removes the shortcut deletes the comment.
- When code and `docs/architecture.md` disagree, the same PR fixes one or the other.

## Docs

Durable decisions live here, not in this file or in issues.

- `docs/architecture.md`: the processes, packages and their import rules, on-disk layout, failure handling and tests.
- `GLOSSARY.md`: the project's vocabulary; use its terms.
- `docs/development.md`: how the project is built, the toolchain and the dev shell.
- `docs/conventions.md`: the definition of done, commits and merging, the PR template, SPDX headers, lint, dependency licenses, Renovate.
- `docs/adr/`: decisions that are hard to reverse.
- `docs/agents/`: how agent skills use the issue tracker, triage labels and domain docs.

## Agent skills

### Issue tracker

Issues are tracked in GitHub Issues for chrishoage/forgejo-runner-plugin-microvm, via the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

Default vocabulary: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `GLOSSARY.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`.
