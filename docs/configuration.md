# Configuration reference

Everything an operator sets for `runner-microvm` is in one YAML file, the config file. This page is the one place its keys are listed: each key, its built-in value (the value it takes when the file leaves it out), what it means, and what makes it invalid. Why each rule is as it is lives in [`architecture.md`](architecture.md) (Configuration).

Examples, each a complete file that the tests run through the same checks as `runner-microvm check`:

- [`examples/config-minimal.yaml`](examples/config-minimal.yaml): a `default` profile and nothing else.
- [`examples/config-plugin-level.yaml`](examples/config-plugin-level.yaml): every plugin-level key.
- [`examples/config-profiles.yaml`](examples/config-profiles.yaml): profiles with scratch paths, mounts and disks, sharing values through a YAML anchor.

## The config file

- **The plugin reads `/etc/runner-microvm/config.yaml`, or the file the environment variable `RUNNER_MICROVM_CONFIG` names.** A relative path in the variable is made absolute once, at startup. `RUNNER_MICROVM_CONFIG` is the only setting of the plugin's own read from the environment: every other setting is a key in the file.
- **No file at the default path, or an empty file (or one holding only comments), means every setting takes its built-in value,** and the plugin logs that it found none.
- **A missing file that `RUNNER_MICROVM_CONFIG` names stops startup,** and so does a file that exists but can't be read, including a symlink, at the default path or on the way to it, whose target is gone.
- **The file must be a regular file:** a FIFO, a device or a directory is a file that can't be read.
- **Every key is optional.** The top level is a mapping of the plugin-level keys and `profiles`; the file holds one YAML document.

## Checking a file

```sh
runner-microvm check
RUNNER_MICROVM_CONFIG=/path/to/new.yaml runner-microvm check
```

`check` reads the file `serve` would read and runs every check `serve` runs on it at startup. It takes no arguments.

- A valid file prints `config <path>: valid` and exits 0; with no file at the default path it prints `no config file at /etc/runner-microvm/config.yaml: every setting takes its built-in value` and exits 0.
- An invalid file prints every error, one per line, and exits 1. Each error names the file, the line, the key and, inside `profiles`, the profile:

  ```
  config /etc/runner-microvm/config.yaml: line 12: profile "big": cpus: must be from 1 to 255, not 0
  ```

- Mount and disk sources are checked on the host `check` runs on, so run it as the plugin's user, on the plugin's host.

Run `check` before a reload or a restart, rather than reading the log afterwards.

## Reload

- **`SIGHUP` reloads the config file** (`systemctl reload` sends it when the unit has `ExecReload=kill -HUP $MAINPID`). Nothing else does: the plugin doesn't watch the file.
- **Only `profiles` reload.** A plugin-level key whose value in the file differs from the running one is logged by name, with both values, as `a changed setting takes effect only at a restart`, and the running value stays until a restart.
- **A reload reads the same file startup read, under the same rules, and validates the whole file.** Its profiles are checked against the running `max_memory_mib`, not a new value in the file.
- **A failed reload keeps the running config entirely,** and logs `reload failed: kept the running config` with the same errors `check` prints. A mount or disk source that is gone fails a reload.
- **A job keeps the copy of its profile taken at Create.** A reload changes only later jobs, never one already created, even one not yet started.

`SIGTERM` and `SIGINT` drain the plugin; a second one ends it at once.

## Plugin-level keys

Read at startup only (see Reload, above).

| Key | Built-in value | Meaning | Valid values |
|---|---|---|---|
| `listen` | `unix:///run/runner-microvm/runner-microvm.sock` | The unix socket the runner connects to: the runner's `plugins.<key>.address`. The socket is mode 0660, so a runner in its group can connect. | `unix://` followed by an absolute path, not `/` |
| `state_dir` | `/var/lib/runner-microvm` | Scratch disks, job logs, kept logs, the job binary and agent | an absolute path |
| `cache_dir` | `/var/cache/runner-microvm` | The image cache | an absolute path |
| `runtime_dir` | `/run/runner-microvm/jobs` | Per-job runtime files: each VM's agent socket | an absolute path |
| `max_memory_mib` | unset | The memory budget: the cap on the sum of `memory_mib` over every running VM. A job waits at Start until it fits. Unset or `0` means no limit. | an integer, 0 or more |
| `cache_max_gib` | `20` | The image cache's size cap, in GiB. `0` keeps no layer that no VM is using. | an integer from 0 to 8589934591 |
| `lib_dir` | unset | The directory libkrun and libkrunfw are loaded from, and only from. Unset loads them by soname through the system's dynamic loader. | an absolute path |
| `libkrun_log_level` | `warn` | libkrun's log level, in each VM's log | `warn`, `info`, `debug` or `trace` |
| `log_level` | `info` | The log level of the plugin, every job process, every VM child and every agent | `debug`, `info`, `warn` or `error` |
| `keep_job_logs` | `false` | `true` makes Remove move each job's logs to `<state_dir>/logs/<environment id>/` instead of deleting them. The plugin never deletes kept logs. | a boolean (see [General rules](#general-rules)) |

- **A plugin-level `0` is taken literally,** except `max_memory_mib`, where `0` means no limit.
- **The directories must not collide.** `state_dir`, `cache_dir` and `runtime_dir` must be distinct, and none may be inside another. The socket's directory (the directory of `listen`'s path) may be `runtime_dir` or its direct parent, as the built-in values have it; otherwise it follows the same rule. The socket may not be one of the directories, and no path may be the `lock` file in one of them. Paths are compared with symlinks resolved. Each error names both keys.
- Missing directories are created at startup; `check` only reads.

## Profiles

`profiles` maps a profile's name to its keys. A label picks a profile; the profile is the VM's shape and storage.

- **A name matches `[a-z0-9][a-z0-9_-]*`.** Any other name could never appear in a label.
- **A label without `;profile=` uses the profile named `default`.** When the file defines no `default`, it is every built-in value; `;profile=default` gets the same.
- **Profiles don't inherit from each other, `default` included.** Each profile is the built-in values plus its own keys. A profile with no keys (`name:` or `name: {}`) is all built-in values.
- **To share values, put a YAML anchor on a profile and merge it into others with `<<`:**

  ```yaml
  profiles:
    default: &base
      cpus: 2
      scratch_paths: [/root/.cache]
    big:
      <<: *base
      cpus: 8          # an explicit key wins over a merged one
  ```

  The decoder expands merges itself; an error in a merged key names the line it is written on and the profile it was merged into. A mapping that merges itself fails, and so do merges that expand to more than 100,000 keys. A key outside the known ones, such as Compose's `x-` keys, is not a place to keep an anchor: put it on a profile.
- **When `max_memory_mib` is set, a profile whose `memory_mib`, its own or the built-in value, is more than it stops startup,** naming both values: no job on it could ever run. The implicit `default` isn't checked, since the file doesn't define it; a job on it that needs more fails at Create.

## Profile keys

Reloaded on `SIGHUP`. A key left out takes its built-in value.

| Key | Built-in value | Meaning | Valid values |
|---|---|---|---|
| `cpus` | `2` | The VM's vCPUs | an integer from 1 to 255 |
| `memory_mib` | `1024` | The VM's memory, in MiB, and the job's share of the memory budget | an integer from 1 to 4294967295 |
| `scratch_gib` | `10` | The size of the scratch disk, fresh for every job, that holds every change the job makes to the VM's root filesystem | an integer from 1 to 8589934591 |
| `scratch_paths` | `[]` | Guest directories kept on the scratch disk instead of the overlay: each starts empty at every boot, hiding what the image has there | a list of guest paths (below), each listed once |
| `mounts` | `[]` | Host directories shared into the VM | a list of entries (below); each source an existing directory |
| `disks` | `[]` | Host files attached to the VM as block devices, each mounted at its target | a list of entries (below); each source an existing regular file |
| `force_pull` | `false` | `true` looks the image's tag up in the registry on every Start, even when the image cache holds it; a registry error then fails the Start | a boolean (see [General rules](#general-rules)) |

- **`0` is invalid for `cpus`, `memory_mib` and `scratch_gib`:** leave the key out to get the built-in value.
- **The upper limits are only the ones past which the VM can't work:** 255 vCPUs and `memory_mib` within a 32-bit count, libkrun's own fields, and a scratch size whose byte count fits a signed 64-bit integer. There is no minimum memory: a VM too small to boot fails Start with its console.
- **At most 3 mounts plus disks in one profile,** on x86_64, the only supported architecture: libkrun gives a VM 11 device interrupt lines, and every VM already uses 8. More stops startup at the later of the `mounts` and `disks` keys.

### Guest paths

A scratch path, and a mount's or disk's target, is a guest path:

- absolute, and cleaned (`/a/./b/` is `/a/b`);
- not `/`, and not `/proc`, `/sys` or `/dev` or under them;
- with no NUL byte.

Scratch paths may be nested (`/a` and `/a/b`).

### Mount and disk entries

Each entry of `mounts` and `disks` is a mapping of these keys:

| Key | Built-in value | Meaning | Valid values |
|---|---|---|---|
| `source` | none: required | The host path: a directory for a mount, a regular file for a disk | an absolute path that exists when the file is read; a symlink to the right kind of file is followed |
| `target` | none: required | Where the guest sees it | a guest path (above) |
| `read_write` | `false` | `true` lets the job write it; `false` shares it read-only, which the guest can't undo, even by remounting | a boolean (see [General rules](#general-rules)) |

Their checks, all made when the file is read:

- **A source must exist,** a directory for a mount, a regular file for a disk; a block device is not a disk source. The job process checks it again at Start, so a source removed since fails that Start, naming it.
- **A target is used once across `mounts` and `disks`, and may not be under another mount or disk target in the same profile.**
- **A target may not be one of the profile's `scratch_paths`, nor above one,** since one would hide the other. A target under a scratch path is fine: mounts and disks are mounted after the scratch paths.
- **A target may not be one of the guest's own directories, nor above one:** the act path `/var/run/act`, the tool cache `/opt/hostedtoolcache` and the temp directory `/tmp`. So `/var`, `/opt` and `/tmp` are refused, and `/opt/tools` or `/opt/hostedtoolcache/go` are fine.
- **A disk source listed twice in one profile must be read-only both times.** Two entries are the same disk when their paths match or they are the same file through a symlink or a hard link.

What the job sees:

- **A disk holds a whole filesystem: ext4 or EROFS read-only, ext4 read-write.** Anything else fails Start. The operator creates and formats the file.
- **Only one job at a time has a read-write disk; read-only users share one.** A job whose disk is in use waits at Start, in arrival order, until it is free, or until it is cancelled or times out. A disk held by a process outside the plugin fails Start with `disk <source> is in use by another process`.
- **A read-write disk's data is flushed when its job ends normally.** A killed VM or plugin may lose its last writes.
- **On a read-write mount, files a job creates as root are owned by the plugin's user when the plugin runs unprivileged;** a job user with another uid can't create files there, and `chown` fails. With the plugin running as root, files get the guest's uid and gid.

## General rules

Each of these stops startup, fails a reload and fails `check`:

- **an unknown key,** at the top level (`x-` keys included), in a profile, or in a mount or disk entry;
- **a key set twice in one mapping,** naming both lines;
- **a key with no value** (`state_dir:`, or `~` or `null` as the value), or **a value of the wrong type:** a quoted number is a string, and a boolean is one of `true`, `True`, `TRUE`, `false`, `False` and `FALSE`, unquoted or tagged `!!bool` (`"true"`, `tRUE`, `yes` and `on` are not);
- **a relative path** in any path key;
- **a YAML syntax error,** with the library's line, **a second YAML document,** or **a top level that isn't a mapping.**

Two keys may have no value: `profiles:` defines no profiles, as leaving it out does, and a profile's name with no value (`name:`) is all built-in values.

Every error in the file is reported, not only the first.

## Labels

A label in the runner's config routes jobs to the plugin and picks their image and profile:

```
<runner label>:<plugins key>://<image>[;profile=<name>]
```

```yaml
runner:
  labels:
    - "microvm:runner-microvm://node:22"
    - "microvm-persist:runner-microvm://registry.example/ci/build:1;profile=persist"
    - "microvm-big:runner-microvm://;profile=big"
plugins:
  runner-microvm:             # the plugins key: the label's scheme
    address: "unix:///run/runner-microvm/runner-microvm.sock"   # listen
```

- **`<plugins key>` is the key under the runner config's `plugins:`,** chosen by the operator. The runner passes the plugin what follows `<plugins key>:`, less a leading `//`.
- **`<image>` is any OCI image reference.** A job's `container:` image replaces it; the label's profile still applies.
- **The image may be empty** (`runner-microvm://;profile=big`): the job must then bring its own `container:` image. A job with no image in either place fails.
- **After the image, only `;profile=<name>` may follow, once.** Anything else, an empty name, a second `;` or a trailing `;` fails the job's Create with `InvalidArgument`, naming the label. Matching is exact and case-sensitive.
- **A profile the file doesn't define fails Create** with `label "<label>": unknown profile "<name>"`, except `default`, which always exists.
- **The runner's `plugins.<key>.options` map is not used.**
