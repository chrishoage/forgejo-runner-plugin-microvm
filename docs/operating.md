# Operator guide

How to install a release of `runner-microvm` beside forgejo-runner on one host and run jobs in it. Follow the sections in order on a fresh host. The plugin's config file has its own reference, [`configuration.md`](configuration.md), which this guide links to rather than repeats; the words it uses are the glossary's ([`GLOSSARY.md`](../GLOSSARY.md)). Why each behaviour is as it is lives in [`architecture.md`](architecture.md).

The commands are for a Debian or Ubuntu host and run as root where they start with `sudo`. The names used throughout are examples: the plugin runs as the user `runner-microvm`, the runner as `forgejo-runner`, release `0.1.0`.

## What the host needs

- **x86_64 Linux with KVM, and read-write `/dev/kvm` for the plugin's user.** The plugin needs nothing else of privilege: no root, no subordinate ids, no setuid helper, no TAP device or bridge. On most distributions `/dev/kvm` belongs to the group `kvm`, so the plugin's user joins it (Users and directories, below). Check, as that user: `test -r /dev/kvm && test -w /dev/kvm && echo ok`.
- **glibc, with its dynamic loader at `/lib64/ld-linux-x86-64.so.2`.** The `runner-microvm` binary is static, but the job binary it writes and runs for each job loads libkrun through the system's dynamic loader, so it is linked against glibc (`libc.so.6`, `libdl.so.2`, `libpthread.so.0`) with that path, the x86_64 standard's, as its loader. Glibc distributions that follow the FHS, such as Debian, Ubuntu and Fedora, have it there. A musl distribution such as Alpine doesn't have it. Without it the plugin stops at startup, when it first runs the job binary, with `run the job binary <state_dir>/bin/runner-microvm-job-<hash> in <state_dir>/bin, the state directory: fork/exec <state_dir>/bin/runner-microvm-job-<hash>: no such file or directory`: the file missing is the loader, not the job binary or the state directory. Check that the loader is there and is glibc's: `test -x /lib64/ld-linux-x86-64.so.2 && ldd --version | head -n 1` prints a line naming glibc, as `GLIBC` (Debian, Ubuntu) or `GNU libc` (Fedora).
- **libkrun 1.19.8 or later, built with block and network support, and libkrunfw 5.6.4 or later,** as the sonames `libkrun.so.1` and `libkrunfw.so.5`. These minimums are the project's `nix/lib-minimums.json`, which each release's notes repeat; a test keeps every version this guide names in step with it. Older libkrun releases corrupt read-write disks, and without block and network support there are no scratch disks and no job network. The plugin loads them by soname through the system's loader, or, with `lib_dir` set, only from that directory ([`configuration.md`](configuration.md#plugin-level-keys)). How to get them is below.
- **`mkfs.ext4` (e2fsprogs) on the plugin's `PATH`.** Each job's scratch disk is formatted with it. The plugin stops at startup, naming it, when it is missing.
- **`net.ipv4.ping_group_range` including the plugin user's group, for `ping` in jobs.** A job's ICMP echo leaves the host through an unprivileged ping socket of the plugin's user; outside the range it is dropped, and everything else works. Check with `sysctl net.ipv4.ping_group_range`: systemd sets `0 2147483647`, every group. Otherwise add a file in `/etc/sysctl.d/` setting it to a range holding that gid.
- **A cache directory on a filesystem with `renameat2`'s `RENAME_NOREPLACE`,** and, with `keep_job_logs: true`, a state directory on one too: ext4, XFS, btrfs and tmpfs have it. On a cache directory without it, every Start that stores an image layer fails, naming the cache directory. Kept job logs are moved into `<state_dir>/logs/` with the same flag, so on a state directory without it every Remove logs `could not keep the job's logs: deleting them`, naming the job and the failed rename, deletes the logs and succeeds; jobs are unaffected. `<state_dir>/logs` must also be on the state directory's filesystem, not a mount of its own: the logs are only renamed, and the same warning then says the directory is on another filesystem.
- **Disk space** for the image cache (`cache_max_gib`, 20 GiB by default) and for each running job's scratch disk (`scratch_gib`, 10 GiB by default, sparse) in the state directory.

### Getting libkrun and libkrunfw

Distribution packages work when they reach both minimums and libkrun is built with block and network support; a package's file names carry its version (`libkrun.so.1.19.8`). Where they don't, install both from upstream into one directory and name it in `lib_dir`:

```sh
# libkrun, from its release tag: a C compiler, make, git and glibc's static library (for libkrun's init),
# libclang (for bindgen, which libkrun's build runs), and Rust's stable toolchain from rustup, for your own
# user (a distribution's cargo may be too old).
sudo apt-get update
sudo apt-get install -y build-essential git curl libclang-dev
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --profile minimal --default-toolchain stable
. "$HOME/.cargo/env"
git clone --depth 1 --branch v1.19.8 https://github.com/libkrun/libkrun.git
make -C libkrun BLK=1 NET=1 PREFIX=/opt/libkrun
sudo make -C libkrun install PREFIX=/opt/libkrun        # /opt/libkrun/lib64/libkrun.so.1.19.8

# libkrunfw, the guest kernel, prebuilt by its release (building it builds a kernel).
curl -fLO https://github.com/libkrun/libkrunfw/releases/download/v5.6.4/libkrunfw-x86_64.tgz
sudo tar -xzf libkrunfw-x86_64.tgz -C /opt/libkrun/lib64 --strip-components=2   # libkrunfw.so.5.6.4

ls -l /opt/libkrun/lib64
```

libkrun names no minimum Rust version; rustup's stable toolchain builds it. The plugin checks both libraries at every startup, before it serves: it logs the files it loaded, or stops with an error naming the library or the functions that are missing, so a libkrun built without block or network support stops it too.

## Install the binary

A release attaches the binary, `runner-microvm-<version>-linux-amd64`, and `SHA256SUMS`, and GitHub keeps a build provenance attestation for both:

```sh
version=0.1.0
base=https://github.com/chrishoage/forgejo-runner-plugin-microvm/releases/download/v$version
curl -fL -O "$base/runner-microvm-$version-linux-amd64" -O "$base/SHA256SUMS"
sha256sum -c SHA256SUMS
gh attestation verify "runner-microvm-$version-linux-amd64" --repo chrishoage/forgejo-runner-plugin-microvm \
  --signer-workflow chrishoage/forgejo-runner-plugin-microvm/.github/workflows/release.yml --source-ref "refs/tags/v$version"
sudo install -m 0755 "runner-microvm-$version-linux-amd64" /usr/local/bin/runner-microvm
runner-microvm version        # 0.1.0
```

- `sha256sum -c` checks the download; `gh attestation verify` checks that this repository's release workflow built it, from the release's tag. `--repo` alone would accept an attestation from any workflow of the repository, on any branch or tag; `--signer-workflow` requires the release workflow and `--source-ref` the tag. `gh` needs a login (`gh auth login`, or `GH_TOKEN`) for it.
- There is no SBOM file: Go records every module the binary is built from in the binary itself, and `go version -m runner-microvm-$version-linux-amd64` prints them.
- **Each release's notes name the forgejo-runner version it was tested with, and the libkrun and libkrunfw minimums.** There is no supported range: the runner's plugin protocol is `v1alpha` and can change between runner releases, so run the runner version the notes name.
- To upgrade, install the new binary over the old one and restart the plugin (Running the plugin, below).

## Users and directories

The plugin and the runner run on one host, each as its own user. The plugin runs as an ordinary user or as root, identically; an ordinary user is what this guide sets up.

```sh
sudo useradd --system --user-group --create-home --groups kvm runner-microvm
sudo useradd --system --user-group --create-home forgejo-runner
sudo usermod --append --groups runner-microvm forgejo-runner   # the socket's group: see below

# The plugin's directories, at their built-in paths.
sudo install -d -m 0700 -o runner-microvm -g runner-microvm /var/lib/runner-microvm /var/cache/runner-microvm
sudo install -d -m 0750 -o runner-microvm -g runner-microvm /run/runner-microvm
```

- **The listen socket is mode 0660, and its group is the plugin process's, or its directory's when that directory is setgid.** So the runner's user needs that group: here it joins `runner-microvm`. Alternatively make the socket's directory setgid to a group the runner's user has (`install -d -m 2750 -o runner-microvm -g forgejo-runner /run/runner-microvm`), which a plugin running as root needs, since its own group is root's. The runner's user must also be able to enter the socket's directory.
- **The plugin creates a missing directory itself at startup,** but an ordinary user can't create these. `/run` is emptied at every boot, so recreate `/run/runner-microvm` before the plugin starts, for example with systemd-tmpfiles or a unit's `RuntimeDirectory=`.
- **Each of the state, cache, runtime and socket directories is locked while a plugin uses it, so one directory set serves one plugin.** A second plugin given a directory in use stops at startup, naming it. Two plugins on one host need a directory set and socket each.

## The plugin's config file

Write `/etc/runner-microvm/config.yaml`, starting from one of the reference's examples ([`configuration.md`](configuration.md)), which each release's tag holds under `docs/examples/`, and check it as the plugin's user, since `check` looks at mount and disk sources as that user sees them:

```sh
examples=https://raw.githubusercontent.com/chrishoage/forgejo-runner-plugin-microvm/v$version/docs/examples
curl -fL -O "$examples/config-minimal.yaml"
sudo install -D -m 0644 config-minimal.yaml /etc/runner-microvm/config.yaml
echo 'lib_dir: /opt/libkrun/lib64' | sudo tee -a /etc/runner-microvm/config.yaml   # only if the libraries aren't on the loader's path
sudo -u runner-microvm runner-microvm check
```

Every label the runner routes to the plugin names a profile this file defines, or none, which gets `default`.

## Running the plugin

```sh
sudo -u runner-microvm runner-microvm serve
```

- **It logs to stderr,** one line per event: its settings (with `version=`), the libraries it loaded, any crash leftovers it deleted, then `serving` once the socket is bound and it takes jobs.
- **Service units are yours to write.** What the plugin needs of one: run `runner-microvm serve` as the plugin's user, with `RUNNER_MICROVM_CONFIG` set if the file isn't at its default path; send SIGHUP to reload; send SIGTERM to the plugin's own process only (systemd: `KillMode=mixed`), so its jobs are drained rather than killed by the service manager; and allow a stop timeout as long as the jobs you want to finish.
- **SIGHUP reloads the profiles** (Reload, in [`configuration.md`](configuration.md#reload)); run `check` first. A running job keeps its profile.
- **SIGTERM or SIGINT drains: the plugin takes no new jobs, serves the jobs it has until the runner removes each one, then exits 0.** A second SIGTERM or SIGINT ends every job at once and exits 0. A new job during the drain fails with `runner-microvm is shutting down`.
- **A restart after a crash cleans up before serving.** A plugin killed without draining (`kill -9`, or a service manager's SIGKILL) takes its jobs' VMs with it, and its next start deletes what they left and logs `deleted crash leftovers`, before it binds the socket.
- **Kept job logs are never pruned.** With `keep_job_logs: true`, each job's logs stay in `<state_dir>/logs/<environment id>/` until you delete them; the Remove log line names the directory. Delete old ones on a schedule, for example `find /var/lib/runner-microvm/logs -mindepth 1 -maxdepth 1 -mtime +14 -exec rm -rf {} +`.

## Configuring the runner

The runner side is three parts of forgejo-runner's own config file. [`examples/forgejo-runner.yaml`](examples/forgejo-runner.yaml) is a complete one, which the tests load with forgejo-runner's own config loader:

- **The label routes jobs to the plugin:** `<runner label>:<plugins key>://<image>[;profile=<name>]`, for example `microvm:runner-microvm://docker.io/library/alpine:3.24`. A workflow's `runs-on: microvm` then runs in a VM of that image, or of its own `container:` image. The syntax and its rules are in [`configuration.md`](configuration.md#labels).
- **`plugins.<plugins key>.address` is the plugin's listen socket,** `unix:///run/runner-microvm/runner-microvm.sock` with the built-in `listen`. The plugins key is the labels' scheme. `plugins.<key>.options` is not used.
- **The runner's cache server must listen on an address the VMs can reach, never loopback,** because a VM can't reach the host's loopback. Set `cache.host` to one of the host's other addresses, or leave it empty and the runner picks the address of its default route; never `127.0.0.1` or `localhost`. Or turn the cache off with `cache.enabled: false`.

Register the runner with Forgejo as forgejo-runner's documentation describes, keeping the labels in `runner.labels` (a `server.connections` entry with labels of its own would replace them), then start it as its user, in its home directory, where a `.runner` registration file is looked for:

```sh
curl -fL -O "$examples/forgejo-runner.yaml"          # then edit its labels and cache.host
sudo install -D -m 0644 forgejo-runner.yaml /etc/forgejo-runner/config.yaml
sudo -u forgejo-runner sh -c 'cd && forgejo-runner --config /etc/forgejo-runner/config.yaml daemon'
```

## Network

- **The plugin does no egress filtering.** A job's connections leave the host as connections of its job process, under the plugin's user, to anything the host can reach, except the host's loopback addresses and link-local addresses (`169.254.0.0/16`, so a cloud host's metadata service is out of reach). The host's other addresses are reachable, and so is every service listening on them.
- **Names are resolved through the host's resolver, from the job process:** the job network's DNS answers nothing itself. So a job's lookups are DNS traffic of the plugin's user too, and a local resolver on the host's loopback, such as systemd-resolved, works.
- **Image pulls are the plugin process's own connections,** under the same user.
- **To restrict egress, filter the plugin user's traffic on the host.** An nftables example, which lets the plugin's user reach the host itself (the loopback interface carries traffic to every local address, the local resolver's included) and Forgejo, and nothing else on private networks, IPv4 or IPv6 unique local (`fc00::/7`):

  ```
  # /etc/nftables.d/runner-microvm.nft; load with: nft -f /etc/nftables.d/runner-microvm.nft
  table inet runner_microvm_egress {
  	chain output {
  		type filter hook output priority filter; policy accept;
  		meta skuid != "runner-microvm" accept
  		oifname "lo" accept
  		ip daddr 192.0.2.10 tcp dport 443 accept   # Forgejo, on another host: replace the address
  		ip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10 } counter reject
  		ip6 daddr fc00::/7 counter reject
  	}
  }
  ```

  Accept as well any registry, resolver or cache server the jobs need on those networks. Jobs are IPv4 only.

## Registries

- **Credentials come from the plugin user's `~/.docker/config.json`, or `$DOCKER_CONFIG/config.json` when the plugin's environment sets it, and the credential helpers it names** (`docker-credential-<name>`, on the plugin's `PATH`). Write it as that user, for example with `docker login`, or by hand.
- **A registry at a localhost, loopback or RFC 1918 address is tried over HTTPS, then over plain HTTP; every other registry is HTTPS only.** On a LAN that is a downgrade: someone on the path to a private-address registry can answer in plain HTTP. Name images there by digest (`registry.lan/ci/build@sha256:…`), which the plugin checks whatever the transport, or keep the registry off private addresses.

## Mounts and disks

What a profile shares with its VMs is described in [`configuration.md`](configuration.md#mount-and-disk-entries); what it needs of the host:

- **Disk files are the operator's to create and format,** ext4 (or EROFS, read-only), and the plugin's user must be able to open them, read-write for a read-write disk. For example:

  ```sh
  sudo truncate -s 10G /srv/vols/persist.ext4
  sudo mkfs.ext4 -q -E root_owner=0:0 /srv/vols/persist.ext4     # the filesystem's root owned by the guest's root
  sudo chown runner-microvm: /srv/vols/persist.ext4
  ```

- **On a read-write mount, files a job creates are owned by the plugin's user when the plugin runs unprivileged;** a job user with another uid can't create files there, and `chown` fails. With the plugin as root, files get the guest's uid and gid.
- **A read-write mount's host directory can receive setuid files and, with the plugin as root, device nodes:** keep it on a filesystem mounted `nosuid,nodev`, or where no other user reaches it.
- **At most 3 mounts plus disks per profile.**

## What the plugin doesn't do

- **Services.** A job that declares `services:` fails at Create: forgejo-runner's plugin protocol passes nothing to configure a service's VM.
- **arm64,** or any architecture but x86_64.
- **Egress filtering** (Network, above).
- **Packages, service units and install tooling:** a release is one binary; how it is installed and supervised is yours.
- **A memory limit for the plugin's own process** is not part of this guide; `max_memory_mib` caps the VMs' memory ([`configuration.md`](configuration.md#plugin-level-keys)).
