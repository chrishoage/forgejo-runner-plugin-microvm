# SPDX-License-Identifier: GPL-3.0-or-later
#
# The local harness: `just e2e up`, `just e2e run`, `just e2e down`. Recipes
# run in e2e/. Each decision and its reason is in docs/development.md (The
# local harness).

# The compose project: one per checkout, so a second checkout's `down` (which
# its `up` runs first) touches only its own stack. Named from a short hash of
# the repo root; COMPOSE_PROJECT_NAME overrides it.
project := env("COMPOSE_PROJECT_NAME", "runner-microvm-e2e-" + replace_regex(sha256(justfile_directory()), '^(.{8}).*$', '$1'))

# The compose command: `docker compose` (the default) or `podman compose`.
compose := env("COMPOSE", "docker compose") + " -f compose.yaml --project-name " + project

# Everything the stack writes, at the repo root and gitignored.
state := justfile_directory() / ".e2e"

# Forgejo's API, published on loopback only.
api := "http://127.0.0.1:3000/api/v1"

# The admin user `up` creates, and the repository `run` creates.
admin := "e2e"
repo := "echo"

# What e2e/workflows/echo.yml's step prints; `run` fails unless a job log
# shows it.
expect := "hello from runner-microvm e2e"

# How long `up` waits for the runner to come online, and `run` for the run to
# end, in seconds.
online_timeout := "120"
run_timeout := "300"

# List the harness's recipes.
[private]
default:
    @just --list e2e

# Start Forgejo and the runner from fresh state; wait until the runner is online.
up: down
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p '{{ state }}/forgejo' '{{ state }}/runner' '{{ state }}/secrets'
    # The runner image's user (uid 1000) writes its registration here,
    # whatever the host's uid or user namespace.
    chmod 0777 '{{ state }}/runner'
    chmod 0755 '{{ state }}/secrets'
    # Throwaway secrets in plain text: they die at `down`.
    hex() { od -An -tx1 -N"$1" /dev/urandom | tr -d ' \n'; }
    secret=$(hex 20)
    password=$(hex 16)
    printf '%s\n' "$secret" >'{{ state }}/secrets/runner-secret'
    printf '%s\n' "$password" >'{{ state }}/admin-password'
    chmod 0644 '{{ state }}/secrets/runner-secret'

    {{ compose }} up --detach --wait forgejo
    {{ compose }} exec -T -u git forgejo forgejo admin user create --admin \
        --username '{{ admin }}' --password "$password" \
        --email '{{ admin }}@example.invalid' --must-change-password=false
    # Offline registration: Forgejo learns the shared secret here, and the
    # runner's container writes its .runner file from the same secret.
    {{ compose }} exec -T -u git forgejo forgejo forgejo-cli actions register \
        --secret "$secret" --name e2e >/dev/null
    {{ compose }} up --detach runner

    # Each try's curl is bounded by the time left, so the wait ends at the
    # deadline, whatever the number of tries.
    echo "waiting for the runner to come online"
    deadline=$((SECONDS + {{ online_timeout }}))
    left() { local r=$((deadline - SECONDS)); echo $((r < $1 ? r : $1)); }
    while [ "$SECONDS" -lt "$deadline" ]; do
        if curl -fsS --max-time "$(left 10)" -u '{{ admin }}':"$password" '{{ api }}/admin/actions/runners' |
            jq -e '.[] | select(.name == "e2e" and (.status == "idle" or .status == "active"))' >/dev/null 2>&1; then
            echo "runner online; Forgejo at http://127.0.0.1:3000 (user {{ admin }}, password in .e2e/admin-password, compose project {{ project }})"
            exit 0
        fi
        sleep 1
    done
    echo "the runner did not come online within {{ online_timeout }}s" >&2
    {{ compose }} logs runner >&2
    exit 1

# Push e2e/workflows/echo.yml to a fresh repository, dispatch it, print the job's log; fail unless it succeeds and the log shows the step's output.
run:
    #!/usr/bin/env bash
    set -euo pipefail
    password=$(cat '{{ state }}/admin-password' 2>/dev/null) || {
        echo "no harness running: run 'just e2e up' first" >&2
        exit 1
    }
    # Every call has a per-call timeout; a later --max-time overrides it.
    call() { curl -fsS --max-time 30 -u '{{ admin }}':"$password" -H 'Content-Type: application/json' "$@"; }
    repo='{{ api }}/repos/{{ admin }}/{{ repo }}'

    # A fresh repository on every run, so no run inherits the last one's.
    curl -sS --max-time 30 -o /dev/null -u '{{ admin }}':"$password" -X DELETE "$repo"
    call -X POST '{{ api }}/user/repos' \
        -d '{"name": "{{ repo }}", "auto_init": true, "default_branch": "main"}' >/dev/null
    jq -n --arg content "$(base64 -w0 workflows/echo.yml)" \
        '{content: $content, branch: "main", message: "ci: add the echo workflow"}' |
        call -X POST "$repo/contents/.forgejo/workflows/echo.yml" -d @- >/dev/null

    run_id=$(call -X POST "$repo/actions/workflows/echo.yml/dispatches" \
        -d '{"ref": "main", "return_run_info": true}' | jq -er .id)
    echo "dispatched run $run_id"

    # Poll until the run ends or the deadline passes. Each poll's curl is
    # bounded by the time left, so the poll ends at the deadline.
    deadline=$((SECONDS + {{ run_timeout }}))
    left() { local r=$((deadline - SECONDS)); echo $((r < $1 ? r : $1)); }
    status=
    while [ "$SECONDS" -lt "$deadline" ]; do
        status=$(call --max-time "$(left 30)" "$repo/actions/runs/$run_id" | jq -er .status)
        case "$status" in
        success | failure | cancelled | skipped) break ;;
        esac
        sleep 1
    done
    echo "run $run_id: $status"

    # A failing call here fails the recipe: an assignment keeps set -e,
    # where a `for … in $(…)` would not.
    job_ids=$(call "$repo/actions/runs/$run_id/jobs" | jq -er '.[].id')
    logs=
    for job_id in $job_ids; do
        log=$(call -H 'Accept: text/plain' "$repo/actions/jobs/$job_id/logs")
        printf -- '--- log of job %s ---\n%s\n' "$job_id" "$log"
        logs+="$log"$'\n'
    done
    case "$status" in
    success) ;;
    failure | cancelled | skipped) echo "run $run_id ended $status" >&2; exit 1 ;;
    *) echo "run $run_id did not end within {{ run_timeout }}s" >&2; exit 1 ;;
    esac
    # The step's output is a log line of its own: a timestamp, then the
    # text. The step's command line also contains the text, but quoted, so
    # it never matches the whole line.
    if ! sed 's/^[^ ]* //' <<<"$logs" | tr -d '\r' | grep -xF '{{ expect }}' >/dev/null; then
        echo "no job log shows the step's output: {{ expect }}" >&2
        exit 1
    fi

# Stop this checkout's stack and delete .e2e/.
down:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ compose }} down --volumes --remove-orphans --timeout 10
    if [ -d '{{ state }}' ]; then
        # Containers write as uids the host user may not own (rootless
        # podman maps them to subordinate uids), so empty the directory
        # from a container, as the container's root.
        {{ compose }} run --rm --no-deps -T --user 0 --volume '{{ state }}:/state' \
            --entrypoint sh forgejo -c 'rm -rf /state/* /state/.[!.]*'
        {{ compose }} down --volumes --remove-orphans --timeout 10
        rm -rf '{{ state }}'
    fi
