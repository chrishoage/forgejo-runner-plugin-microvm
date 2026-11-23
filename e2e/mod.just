# SPDX-License-Identifier: GPL-3.0-or-later
#
# The local harness: `just e2e up`, `just e2e run [workflow]`, `just e2e
# reload`, `just e2e down`. Recipes
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

# The admin user `up` creates. `run` creates a repository named after the
# workflow it runs.
admin := "e2e"

# What the first step of each workflow in e2e/workflows/ prints; `run` fails
# unless a job log shows it.
expect := "hello from runner-microvm e2e"

# How long `up` waits for the runner to come online, and `run` for the run to
# end, in seconds.
online_timeout := "120"
run_timeout := "300"

# The plugin, built by `up` and run on the host with its directories under
# .e2e/plugin, set by its config file, with its pid and log beside them.
plugin := justfile_directory() / "bin/runner-microvm"
plugin_dir := state / "plugin"
plugin_config := state / "plugin-config.yaml"
plugin_pid := state / "plugin.pid"
plugin_log := state / "plugin.log"

# List the harness's recipes.
[private]
default:
    @just --list e2e

# Build and start the plugin, then Forgejo and the runner, from fresh state; wait until the runner is online.
up: down
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p '{{ state }}/forgejo' '{{ state }}/runner' '{{ state }}/secrets'
    chmod 0755 '{{ state }}/secrets'

    # The plugin runs on the host, for /dev/kvm and libkrun, in the
    # background, so an agent needs no second terminal. Its pid and log
    # are in .e2e/; `down` stops it.
    just --justfile '{{ justfile_directory() }}/justfile' build
    # Its config file puts its directories under .e2e/plugin, with the
    # listen socket in the runtime directory, which the runner's container
    # mounts. It defines default and a larger profile, big, which the
    # microvm-big label names; each key on a line of its own, so an edit
    # for a reload is one sed.
    sock='{{ plugin_dir }}/runtime/runner-microvm.sock'
    {
        echo "listen: unix://$sock"
        echo "state_dir: {{ plugin_dir }}/state"
        echo "cache_dir: {{ plugin_dir }}/cache"
        echo "runtime_dir: {{ plugin_dir }}/runtime"
        echo "profiles:"
        echo "  default:"
        echo "    cpus: 1"
        echo "    memory_mib: 768"
        echo "    scratch_gib: 2"
        echo "  big:"
        echo "    cpus: 2"
        echo "    memory_mib: 1536"
        echo "    scratch_gib: 4"
    } >'{{ plugin_config }}'
    export RUNNER_MICROVM_CONFIG='{{ plugin_config }}'
    '{{ plugin }}' check
    setsid '{{ plugin }}' serve >'{{ plugin_log }}' 2>&1 </dev/null &
    echo $! >'{{ plugin_pid }}'
    deadline=$((SECONDS + 30))
    until [ -S "$sock" ]; do
        if ! kill -0 "$(cat '{{ plugin_pid }}')" 2>/dev/null || [ "$SECONDS" -ge "$deadline" ]; then
            echo "the plugin did not start; its log:" >&2
            cat '{{ plugin_log }}' >&2
            exit 1
        fi
        sleep 0.1
    done
    echo "plugin serving on $sock (pid $(cat '{{ plugin_pid }}'), log .e2e/plugin.log)"
    # The runner's config, with the label's image from e2e/image.
    image=$(tr -d '[:space:]' <image)
    sed "s|@IMAGE@|$image|" runner-config.yaml >'{{ state }}/runner-config.yaml'
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

# Push e2e/workflows/<workflow>.yml to a fresh repository of that name, dispatch it, print the job's log and the VM's shape; fail unless it succeeds and the log shows the first step's output.
run workflow="echo":
    #!/usr/bin/env bash
    set -euo pipefail
    file='workflows/{{ workflow }}.yml'
    [ -f "$file" ] || {
        echo "no workflow e2e/$file" >&2
        exit 1
    }
    password=$(cat '{{ state }}/admin-password' 2>/dev/null) || {
        echo "no harness running: run 'just e2e up' first" >&2
        exit 1
    }
    # Every call has a per-call timeout; a later --max-time overrides it.
    call() { curl -fsS --max-time 30 -u '{{ admin }}':"$password" -H 'Content-Type: application/json' "$@"; }
    repo='{{ api }}/repos/{{ admin }}/{{ workflow }}'

    # A fresh repository on every run, so no run inherits the last one's.
    curl -sS --max-time 30 -o /dev/null -u '{{ admin }}':"$password" -X DELETE "$repo"
    call -X POST '{{ api }}/user/repos' \
        -d '{"name": "{{ workflow }}", "auto_init": true, "default_branch": "main"}' >/dev/null
    jq -n --arg content "$(base64 -w0 "$file")" \
        '{content: $content, branch: "main", message: "ci: add the {{ workflow }} workflow"}' |
        call -X POST "$repo/contents/.forgejo/workflows/{{ workflow }}.yml" -d @- >/dev/null

    run_id=$(call -X POST "$repo/actions/workflows/{{ workflow }}.yml/dispatches" \
        -d '{"ref": "main", "return_run_info": true}' | jq -er .id)
    echo "dispatched run $run_id of {{ workflow }}"

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
    echo "run $run_id of {{ workflow }}: $status"

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
    lines=$(sed 's/^[^ ]* //' <<<"$logs" | tr -d '\r')
    if ! grep -xF '{{ expect }}' <<<"$lines" >/dev/null; then
        echo "no job log shows the step's output: {{ expect }}" >&2
        exit 1
    fi
    # The VM's shape, as each shape step printed it: its own line, like the
    # step's output above.
    grep -E '^shape: ' <<<"$lines" | sed 's/^/run {{ workflow }}: /' || true

# Send the plugin SIGHUP, so it reloads .e2e/plugin-config.yaml; print the reload's log lines, and fail if it failed.
reload:
    #!/usr/bin/env bash
    set -euo pipefail
    pid=$(cat '{{ plugin_pid }}' 2>/dev/null) || {
        echo "no harness running: run 'just e2e up' first" >&2
        exit 1
    }
    # Only the log written after the signal: a reload logs its profiles,
    # then its summary line last, or one error line.
    from=$(($(wc -l <'{{ plugin_log }}') + 1))
    kill -HUP "$pid"
    deadline=$((SECONDS + 30))
    while [ "$SECONDS" -lt "$deadline" ]; do
        new=$(tail -n +"$from" '{{ plugin_log }}')
        if grep -q 'msg="reload failed' <<<"$new"; then
            grep 'msg="reload failed' <<<"$new" >&2
            exit 1
        fi
        if grep -q 'msg="reloaded the config file"' <<<"$new"; then
            grep -E 'msg=(profile|"reloaded the config file"|"a changed setting)' <<<"$new"
            exit 0
        fi
        sleep 0.1
    done
    echo "the plugin logged no reload within 30s; its log is .e2e/plugin.log" >&2
    exit 1

# Stop this checkout's stack and plugin, and delete .e2e/.
down:
    #!/usr/bin/env bash
    set -euo pipefail
    {{ compose }} down --volumes --remove-orphans --timeout 10
    # SIGTERM: the plugin removes every job, ending its VMs, then exits.
    if [ -f '{{ plugin_pid }}' ]; then
        pid=$(cat '{{ plugin_pid }}')
        if kill -TERM "$pid" 2>/dev/null; then
            deadline=$((SECONDS + 30))
            while kill -0 "$pid" 2>/dev/null; do
                if [ "$SECONDS" -ge "$deadline" ]; then
                    # Each job process leads a process group of its own,
                    # which killing the plugin's misses: kill each child's
                    # group while the plugin still runs, then the plugin's.
                    echo "the plugin did not stop on SIGTERM; killing it and its job processes" >&2
                    for child in $(cat /proc/"$pid"/task/*/children 2>/dev/null); do
                        # The group is /proc's stat field 5, after the
                        # parenthesised name.
                        stat=$(cat /proc/"$child"/stat 2>/dev/null) || continue
                        read -r _ _ pgid _ <<<"${stat##*) }"
                        if [ -n "$pgid" ] && [ "$pgid" != "$pid" ]; then
                            kill -KILL -- "-$pgid" 2>/dev/null || true
                        fi
                        kill -KILL "$child" 2>/dev/null || true
                    done
                    kill -KILL -- "-$pid" 2>/dev/null || true
                    break
                fi
                sleep 0.1
            done
        fi
    fi
    if [ -d '{{ state }}' ]; then
        # Containers write as uids the host user may not own (rootless
        # podman maps them to subordinate uids), so empty the directory
        # from a container, as the container's root.
        {{ compose }} run --rm --no-deps -T --user 0 --volume '{{ state }}:/state' \
            --entrypoint sh forgejo -c 'rm -rf /state/* /state/.[!.]*'
        {{ compose }} down --volumes --remove-orphans --timeout 10
        rm -rf '{{ state }}'
    fi
