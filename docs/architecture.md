# Architecture

How `runner-microvm` is put together: its processes, what flows between them, where code goes and which imports are forbidden, what it leaves on disk, how it fails and how each part is tested. Each decision carries its reason. The terms are the glossary's (`GLOSSARY.md`). The decisions that are hardest to reverse also have an ADR:

- [ADR 0002](adr/0002-job-process-with-vm-child.md): a job process per job, with libkrun in a VM child.
- [ADR 0003](adr/0003-plugin-talks-to-agent-directly.md): the plugin talks to the agent directly.
- [ADR 0004](adr/0004-libkrun-via-purego.md): libkrun is bound with purego.
- [ADR 0005](adr/0005-image-layers-are-erofs-files-on-one-virtiofs-share.md): image layers are EROFS files shared over one virtiofs share.

When the code and this document disagree, the same PR fixes one or the other.

## Today's limits are the plugin protocol's

forgejo-runner's plugin protocol passes the plugin a job's image, `cap_add` and `cap_drop`, its services (name, image, env, ports), the runner's per-label `backend_options`, the label argument and a timeout, and nothing else of the workflow's `container` and `services` spec. Whatever the plugin can't do today for that reason is a limit of the protocol, not a rule of this architecture: this document says so as "today, because the plugin protocol …", and shapes formats and seams so that lifting the limit adds entries or fields, not a redesign. As the protocol passes more, the plugin takes up whatever makes users' lives easier; what that looks like is decided then, not here.

## Programs

The repo builds three programs, each from a thin `main` under `cmd/`:

| Program | Runs on | What it is |
|---|---|---|
| `runner-microvm` | the host | the plugin: serves the runner, owns what jobs share (image cache, queue, memory budget) |
| `runner-microvm-job` | the host | the job binary: the job process, and with `vm` a VM child |
| `runner-microvm-agent` | inside the VM | the agent: boot mounts, Exec, CopyIn, CopyOut |

The job binary and the agent are embedded in `runner-microvm`, so there is one file to ship and the three can't drift apart in version.

## Process model

```
             runner (forgejo-runner)
                │  BackendPlugin gRPC, unix socket
                ▼
 ┌─────────────────────────── plugin ───────────────────────────┐
 │ gRPC server · job table · image cache · queue · memory budget │
 └──┬───────────────────────────────────────────────────┬───────┘
    │ control channel                                   │ agent protocol (gRPC)
    │  stdin:  job spec (protodelim)                    │  over <runtime>/<id>/<n>/ agent socket,
    │  stdout: status events (protodelim)               │  which libkrun maps to the guest's
    │  stderr: job process log file                     │  vsock port
    │  fd 3:   liveness pipe                            │
    ▼                                                   │
 ┌──────────── job process (one per job) ────────────┐  │
 │ job network (gvisor-tap-vsock) · disk locks       │  │
 └──┬────────────────────────────────────────────────┘  │
    │  stdin:  VM child spec (protodelim)               │
    │  fd 3:   liveness pipe                            │
    │  fd 4:   its VM's end of the job network socket   │
    │  fd 5+:  each read-write disk's locked file       │
    │  stdout, stderr: its VM's log file                │
    ▼                                                   │
 ┌───── VM child (job binary `vm`), one per VM ──────┐  │
 │ libkrun (purego) → VM; console → console file     │  │
 │   ┌──────────────── VM ─────────────────────┐     │  │
 │   │ agent ◄─────────────────────────────────┼─────┼──┘
 │   │  └─ steps, in the job's container       │     │
 │   └─────────────────────────────────────────┘     │
 └───────────────────────────────────────────────────┘
```

### One job process per job, with libkrun in a VM child

- **Each job has one job process, which owns everything belonging to it:** its job network, its VM children, its disk locks and its files. Ending the job process ends the job. The plugin keeps only what jobs share.
- **libkrun runs in a VM child the job process starts, one for each VM, not in the job process itself.** `krun_start_enter` never returns, and libkrun ends the whole process when the guest stops. In a child, libkrun's Rust threads never share an address space with the Go code that owns the job network and the disk locks. The job process also outlives its VMs, so it can report why each one stopped. See [ADR 0002](adr/0002-job-process-with-vm-child.md).
- **The VM child is the job binary run with a `vm` subcommand.** The plugin passes the job binary's own path in the job spec, so nothing ever execs `os.Executable()`, and a test binary can't recurse.
- **Each VM child's inputs:**
  - stdin: its VM child spec, one length-delimited protobuf message, in a memory file (`memfd`), not a pipe, so no goroutine of the job process copies it in and a VM child that never reads it, or a process it left holding stdin, can't keep the job process waiting;
  - fd 3: the liveness pipe;
  - fd 4: its VM's end of the job network socket;
  - fd 5 and up: each of its VM's read-write disks' locked file, so the lock lasts as long as any process that can write the disk;
  - stdout and stderr: its VM's log file. libkrun's console goes to its VM's console file.

### One job process runs all of a job's VMs

- **One job process runs every VM of its job, each in its own VM child, all on the one job network:** the job's container, and each of its services once forgejo-runner's plugin protocol can pass options to configure them. The job spec holds what is job-wide (the job, the job binary, the library directory and the job network) and a list of VMs, each with its own shape, scratch disk, log and console files, agent socket, root and layer link directory, mounts and disks, address and MAC on the job network, and boot config. A service brings its own image and options, so none of these can be job-wide.
- **The first VM in the job spec is always the job's container.** The plugin's Exec and Copy go to its agent, because the plugin protocol's Exec and Copy name only the job, not a VM. A VM is ready when its agent answers Ping; today the job's container is the only VM, because the plugin protocol passes no VM options for services, so its agent is the only one the plugin dials.
- **A VM is identified by its index in the job spec: 0 is always the job's container, and services follow, in order.** The index names the VM's directories (On-disk layout, below), its key in the image cache seam (`Acquire`), and the VM in status events. An index is chosen by the plugin, so a workflow can't make two VMs collide or make a path leave its directory.
- **Each VM also has a name, unique within the job: its name in `Network.hosts`, log lines and `Failed` reasons.** It is never a path component or a cache key, because a service's name would be its DNS name on the job network: its key in the workflow, which the workflow chooses and which may be `job`, `0` or `../x`. Today the job's container's name is the job's name made DNS-safe, as for its hostname (GST-12, the agent protocol's boot config).
- **The job's container exiting ends the job.** The job process then kills the other VM children, reports each one's `VMExited` once it is reaped, and exits. What a service VM exiting does to the job is decided with services.
- **A read-write disk named twice in one job fails Create, with `disk <source> is attached read-write more than once in job <id>`.** One process can't hold two `flock`s on one file (two opens in one process block each other), so the job process couldn't lock it for both VM children, and two VMs writing one block device corrupt it. Read-only disks may be shared between a job's VMs, because they aren't locked (STO-8).
- **Today the job spec always holds exactly one VM, the job's container,** because the plugin protocol passes a service only its name, image, env and ports, and no VM options: a job that declares services fails at Create (PROTO-6, a limit of today's plugin protocol), and the job process rejects a spec whose VM count isn't one. The shape is built for more: services become more VMs of the same job, each another entry in the job spec's VMs, on the same job network and in the same job process, and that shape needs no redesign. What a service needs beyond it is added fields and calls, for example its entrypoint and `cmd` at boot and the plugin dialling its agent. Where a service VM's shape and storage come from is decided when services are.

### Children die with their parent

- **Every child gets a liveness pipe and `Pdeathsig: SIGKILL`.** The pipe is the primary signal: its closing tells a job process or VM child that the plugin is gone. Pdeathsig is the backstop, because the kernel kills the child even when its Go code is stuck in libkrun.
- **Children are forked from a dedicated, locked OS thread.** Pdeathsig fires when the forking *thread* exits, not the process, so a goroutine that happened to fork on a short-lived thread would kill its child early.

### The control channel: plugin ↔ job process

- **The control channel carries only a spec in and status events out:**
  - stdin: the job spec;
  - stdout: status events: network up, then for each VM, VM started and VM exited (with its exit code and console tail), each naming the VM by its index in the job spec; failed (with a reason);
  - both as length-delimited protobuf (`protodelim`);
  - stderr: the job process log file;
  - fd 3: the liveness pipe.
- **It is not gRPC,** because one message in and a handful out don't need a server.
- **No spec appears on a command line,** neither the job spec nor the VM child spec. A command line is visible to every user on the host, and the spec may carry values that must not leak (LOG-2).

### The plugin talks to the agent directly

- **The plugin dials the agent over the unix socket libkrun maps to the guest's vsock port,** in the VM's runtime directory, `<runtime>/<id>/<n>/`. Exec and Copy go to the job's container's agent, because the plugin protocol's Exec and Copy name only the job, not a VM. The job process relays no streams, so there is no streaming relay to write and maintain. See [ADR 0003](adr/0003-plugin-talks-to-agent-directly.md).
- **A VM is ready when its agent answers Ping,** which the plugin learns by dialling the agent, not from a job process event. Today the job's container is the only VM, because the plugin protocol passes no VM options for services, so its agent is the only one the plugin dials.
- **The agent protocol is gRPC.** Cancelling a stream cancels the agent's context, which kills the step's process group (GST-8) with no extra code, and gRPC is already a dependency through the runner's protocol.
- The vsock port and the readiness details (timeouts, polling) are #6's.

### Ending a job: the whole process group, reaped

- **Each job process leads its own process group, and its VM children stay in it.** Killing a job is one `SIGKILL` to that group.
- **The plugin is a child subreaper (`PR_SET_CHILD_SUBREAPER`, set once at startup).** A VM child is the job process's child, not the plugin's. Without the subreaper, a VM child whose job process is killed is reparented to init, nothing in the plugin can wait for it, and it still holds its disk `flock` (fd 5+) while the kernel frees its guest memory. As subreaper, the plugin inherits it and reaps it.
- **A job's disks are freed only once every process in its group has exited and been reaped.** When the job process exits, killed or on its own, the launcher sees it with `waitid(WNOWAIT)` without reaping it, so the group id can't be reused; then it sends `SIGKILL` to the group, reaps the group's processes (`wait4(-pgid)` until no child in the group is left), and only then closes `Exited`. `Kill` returns only after that. So Remove frees the job's disks in the queue (QUE-7) only when no process can still hold their locks, and the next job waiting on a disk (QUE-3) never fails Start with `in use`. The liveness pipe and Pdeathsig say when a VM child must die, not when it has, and a job process killed by `SIGKILL` can't wait for its own child, so neither gives this guarantee.

### A failed Start

- **A failed Start kills the job process's group at once and waits until it is reaped.** That drops its VMs, job network and disk locks without waiting for the runner's Remove.
- **A job whose Start failed is never started again.** A second Start fails with `FailedPrecondition`, naming the first failure: the failed Start left the VM's files half made, and the runner removes such a job rather than starting it again.
- **Remove then only deletes files and frees queue places,** and does nothing harmful when the job process is already gone. A job's disks are still freed exactly once (QUE-7), and only after its group is gone.

### The scratch disk

- **The job process creates each VM's scratch disk in `<state>/jobs/<id>/<n>/`, before it starts the VM child: a sparse file of the VM's scratch size, mode 0600.** Its size is apparent, so the file grows only as the guest writes. The job spec already says the job process creates it, and it is the job's file, deleted with the job's state directory at Remove.
- **It is formatted with the host's `mkfs.ext4 -q -F -E lazy_itable_init=1,lazy_journal_init=1,root_owner=0:0`,** which takes about 50 ms and allocates about 4 MB. The plugin looks `mkfs.ext4` up on `PATH` at startup, stops with an error naming it when it is missing, and passes its path in the job spec, so the job process runs the binary startup checked. This makes e2fsprogs a host dependency (`docs/development.md`). Formatting in the guest would need a mkfs the image may not have, or an ext4 writer of our own; an embedded template would need `resize2fs` for each profile's size anyway.
- **The file must not exist yet, and a disk that can't be made is deleted.** A failure is a `Failed` event naming the scratch disk (PROTO-10). A fresh file for each VM is why nothing written in one job is visible in the next (VM-5).
- **It is the VM's first block device, `/dev/vda`, read-write.** It is not locked: no other process ever names it.
- **Its size is the built-in 10 GiB until #8's profiles set it.**

## libkrun

- **libkrun is bound with purego, not cgo, and only the VM child loads it.** With cgo, libkrun is linked when the binary loads, so `RUNNER_MICROVM_LIBDIR` (VM-10) would need `LD_LIBRARY_PATH` and a missing library couldn't give a clean error. purego `dlopen`s from the named directory, and the libkrun 1.x calls take only ints and strings. See [ADR 0004](adr/0004-libkrun-via-purego.md). The plugin makes a relative `RUNNER_MICROVM_LIBDIR` absolute at startup, because the VM child changes its working directory before it loads libkrun.
- **Every binary builds with `CGO_ENABLED=0`.**

## Job network

- **The job network is gvisor-tap-vsock, run inside the job process.** Its stack isn't freed on `Close`, which is harmless because the job process ends with the job. The alternatives, passt and slirp4netns, are separate C binaries, which breaks the single-binary decision.
- **Its DHCP and HTTP API stay off** (NET-6). Port forwarding is off today: the only ports the plugin protocol passes are a service's `ports`, and a job that declares services fails at Create.

## Extracting the job binary and agent

- **The plugin writes the job binary to `<state>/bin/runner-microvm-job-<first 12 hex of its sha256>`, and the agent to `<state>/bin/runner-microvm-agent-<first 12 hex of its sha256>`.** The state directory is less often `noexec` than `/run`. The content hash makes upgrades clean: once both are written, every other file in `<state>/bin/` (stale hashes, a crashed write's temp files) is deleted, since the plugin holds every directory lock.
- **It always rewrites both at startup, even when a file of that name exists:** a temp file in `<state>/bin/`, fsync, rename, then fsync of `<state>/bin/`. A file is never trusted by its name: a power loss after a rename without those fsyncs can leave an empty file under the right name, and `version` would then fail with a misleading error.
- **At startup the plugin runs the extracted job binary once, with `version`.** If that fails, startup stops with an error naming the directory and its config key. There is no fallback: a `memfd` exec can be blocked by sysctl hardening, and it is more code.
- **The agent is written once per plugin, and each VM's boot root holds a hard link to it, not a copy** (The boot root, below). The state directory is one filesystem, so the link always works.

### The embedded payload

- **The embedded payload is a build-time guarantee.** One file in `internal/payload`, behind the `payload` build tag, embeds the job binary and the agent by exact file name, so a missing binary is a compile error.
- **Without the tag, a stub returns `ErrNoPayload`,** so plain `go build`, `go vet`, `go test` and lint work without building the payload first.
- **Release and `kvm` builds pass `-tags payload`.**
- **`just build` always builds the job binary and the agent first, with `-trimpath -buildvcs=false`,** so the plugin binary is reproducible under Nix.
- **The built payload is not committed.**

## The boot root and the guest's root

[ADR 0005](adr/0005-image-layers-are-erofs-files-on-one-virtiofs-share.md) records why the guest's root is built this way.

- **Each VM boots from its own small boot root, `<state>/jobs/<id>/<n>/root/`, which libkrun shares read-only as the VM's root** (`krun_add_virtiofs3` with the root tag `/dev/root`). It holds empty `proc`, `sys` and `dev` mount points, so libkrun's init can mount them and images without them boot (GST-2). It also holds the agent, as a hard link, and the agent's boot config. It is the bottom layer of the guest's root, so the agent and its config stay visible in the job's root.
- **The boot config is a file in the boot root, `/.runner-microvm-boot`: the agent protocol's `BootConfig` message, as binary protobuf, mode 0600.** The agent is at `/.runner-microvm-agent`. Both names and the encoding are provisional picks, marked `SHORTCUT(#6)`, for #6 to settle with the rest of the agent's startup. The file holds the image's environment, which may carry values.
- **The VM gets two read-only virtiofs shares: its boot root as its root, and its layer link directory, under the tag `layers`** (provisional, `SHORTCUT(#6)`). Read-only is enforced on the host by libkrun's flag, so the guest can't write a layer even after remounting the share read-write (IMG-9). Each VM can read only its own image's layers, because its share is its own link directory (IMG-8). The layers take one share, not one device each: libkrun allows 3 mounts plus disks per VM on x86_64, which rules out a block device per layer, and flattening the image would lose sharing between images.
- **The agent builds the root at boot,** in `/dev/.runner-microvm/`, because devtmpfs is the one writable place before the root exists:
  1. It mounts the scratch disk (ext4).
  2. It mounts the layer share read-only, at the boot config's `layer_dir`.
  3. It mounts each layer file as a file-backed EROFS (`source=` the file, no loop device). This needs `EROFS_FS_BACKED_BY_FILE`, which libkrunfw 5.6.4 has.
  4. It builds an overlay with `fsopen` and one `lowerdir+` per layer, top layer first, with the boot root (the VM's `/` at boot) at the bottom. `upperdir` and `workdir` go on the scratch disk. Overlay's stacking limit, 500, covers GST-3's 120 layers, and the image cache refuses an image of more than 499. `lowerdir+` needs kernel 6.8 or later; libkrunfw 5.6.4 is 6.12. Unlike `mount(2)`'s option string, it has no page-size limit on the layer list.
  5. It makes its mounts in the new root: `proc`, `sysfs`, `cgroup2`, `devtmpfs`, `devpts` (`gid=5,mode=620,ptmxmode=666`) and a `tmpfs` on `/dev/shm`, all `nosuid`, most `nodev,noexec` (#1's prototype set, provisional for #6), creating each mount point in the overlay if the image lacks it.
  6. It moves the overlay to `/` and chroots. The boot root, the shares and the layers stay mounted beneath it, out of the job's reach.

  The layer list and the scratch device come from the boot config fields the agent protocol already defines. The agent's new mount API calls are raw system calls, because the agent may not import `golang.org/x/sys`.
- **A failure to build the root ends the agent, so the VM exits during boot,** and Start fails with its console tail (VM-8).

## On-disk layout

`<state>`, `<cache>` and `<runtime>` are the plugin's directories, and `<listen>` is the directory holding its listen socket; each has its own config key (CFG-9). `<id>` is the job's environment id, and `<n>` is the VM's index in the job spec's VMs: 0 for the job's container, then each service in order. Today the job's container is the only VM, because the plugin protocol passes no VM options for services. A VM's directories are named by its index, never by its name, because the workflow chooses a service's name (One job process runs all of a job's VMs, above). Each VM gets its own directories, so a job's VMs never share a file by accident; the job process log is the job's, so it stays at the job's level.

| Path | Holds |
|---|---|
| `<state>/lock`, `<cache>/lock`, `<runtime>/lock`, `<listen>/lock` | the directory locks |
| `<state>/bin/` | the job binary and the agent |
| `<state>/jobs/<id>/` | the job process log |
| `<state>/jobs/<id>/<n>/` | the VM's scratch disk, VM log and console |
| `<state>/jobs/<id>/<n>/root/` | the VM's boot root: `proc`, `sys` and `dev` mount points, a hard link to the agent, and the boot config |
| `<state>/logs/<id>/` | kept logs (whether to keep them is #11's) |
| `<runtime>/<id>/<n>/` | the VM's agent socket |
| `<listen>/` | the listen socket |
| `<cache>/layers/<diffID hex>.erofs` | one EROFS file per layer, shared by every image and VM that uses it; its mtime is its last use |
| `<cache>/images/<manifest digest hex>.json` | an image's raw config |
| `<cache>/tags/<sha256 hex of the normalised reference>` | a tag file: the normalised reference and the manifest digest it resolved to |
| `<cache>/jobs/<id>/<n>/` | the VM's layer link directory: `0.erofs`, `1.erofs`, …, hard links to its image's layers, bottom first |
| `<cache>/tmp/` | partial writes |

`<state>`, `<cache>` and `<runtime>` are distinct directories, none inside another; `<listen>` is either `<runtime>` or a fourth such directory (see below). `<state>` and `<cache>` are mode 0700. Image cache, below, says why the cache is laid out as it is.

## Image cache

The plugin process pulls and converts images, each layer pull in its own goroutine, and makes and removes each VM's layer link directory, so the job process receives a ready one (#1, Image store).

- **go-containerregistry fetches manifests and layers; go-erofs's `erofs.Create` with continuity's `tarconv.Apply` converts them.** They are maintained libraries, and conversion needs no privilege and no host EROFS support (IMG-4). Both are Apache-2.0. tarconv exists only in unreleased commits, so both are pinned to pseudo-versions, which Renovate updates as digests in its Go group.
- **A layer is converted from its uncompressed stream straight into an uncompressed EROFS file.** No compressed blob is kept: the file is the cache. The image cache decompresses the blob itself, gzip or zstd told apart by its first bytes as go-containerregistry does, in the conversion's own goroutine: go-containerregistry decodes zstd in goroutines of its own, where a panic would end the plugin, so zstd runs with a decoder concurrency of 1, which decodes in the reader's goroutine. The zstd decoder is klauspost/compress's, the one go-containerregistry already depends on, now imported directly. tarconv turns OCI whiteouts into overlay whiteouts (a char device 0:0) and opaque directories into `trusted.overlay.opaque=y`.
- **A layer is named by its diffID, the digest of its uncompressed content: `<cache>/layers/<diffID hex>.erofs`.** The diffID doesn't depend on the registry's compression, so a layer several images share is stored once (IMG-5). The file's presence is the index: there is no index file to keep in sync.
- **The stream is hashed as it converts, and a layer whose content isn't its diffID fails the Start.** The diffID comes from the image config, which the image's author chooses: without the check, an image could name a popular base layer's diffID with other content, and every image using that layer would boot it.
- **The diffID must be a sha256 digest in lowercase hex before it becomes a file name,** and an image may have at most 499 layers: overlay stacks at most 500 lower layers, and the boot root is one of them.
- **Every write goes to `<cache>/tmp/`, then fsync, rename into place, and fsync of `<cache>/layers/`,** the same rule as the job binary. A power loss after a rename without those fsyncs can leave an empty `.erofs` under its real name, which would then count as cached forever. The rename never replaces a file already there (`RENAME_NOREPLACE`): another Start stored the same content first, and VMs may already link it. The cache directory's filesystem must support it (`docs/development.md`); one that doesn't fails each Start that stores a layer, with an error naming the cache directory and the missing support.
- **An image's missing layers all convert before any is renamed into place,** so a layer that fails leaves nothing in the cache, not even the image's other new layers (IMG-10). Every layer is looked up in the cache before any conversion starts, so a lookup that fails starts nothing. The first layer to fail cancels the others' reads, and the Start returns only once every conversion it started has ended and its temp file is gone.
- **The cache's directories are mode 0700, and its files 0600.** The cache holds image configs, whose environment may carry values. Only the plugin's own user needs access; the same user runs libkrun's virtiofs server.
- **A panic in a layer's decompression or conversion is recovered inside that conversion, and fails only that job's Start (IMG-11).** A pull runs inside the plugin process, so an unrecovered panic would end every job.
- **A layer may hold at most 1,000,000 tar entries and 256 MiB of names, link targets and PAX records (xattrs among them); a layer over either fails that Start with an error naming the limit, and leaves nothing in the cache.** go-erofs keeps every entry's metadata in memory until the layer's file is written, and a layer is untrusted: a small one with millions of entries could exhaust the plugin's memory and end every job. The largest real images hold a few hundred thousand files across all their layers, with paths averaging well under 100 bytes, so both limits are generous. They are counted on a copy of the tar stream as it converts, so the conversion stops at the limit rather than after it.
- **Running out of space mid-conversion fails that Start with an error naming the cache directory, and the temp file is deleted.** Nothing triggers an emergency eviction: the size cap is the operator's tool.
- **The plugin sets no timeout of its own on a stalled pull.** It ends when the job is cancelled or its runner or job timeout expires, as QUE-6 says for every wait. No memory limit for the plugin is documented.
- **One plugin-wide limit of 3 layer pulls at once.** A limit per image stops being a limit once several cold images pull together.
- **A hard link to a file in an earlier layer fails Start with `HardLinkError`, saying the image cache does not support it, and leaves nothing in the cache (IMG-10).** tarconv has no typed error for it, so the image cache matches tarconv's `unresolved hard link "<name>"` text and wraps it in its own typed error; a test pins the text, so a tarconv upgrade that changes the wording fails CI. If tarconv ever gains a typed error, the cache switches to it; the project never depends on that happening.
- **Each VM's layer link directory, `<cache>/jobs/<id>/<n>/`, holds one hard link per layer of its image, named by its position, bottom layer first (`0.erofs`, `1.erofs`, …).** Hard links are what `Image.LinkDir` promises, and they can't cross filesystems, which is why the directory is under the cache. Names come from positions, not diffIDs, because an image may hold the same layer twice. A layer with more than one link is in use. The image cache doesn't evict yet (#44), but it updates a layer's mtime as it links it, so the last use is recorded from the start. A failed Acquire deletes the link directory it began.
- **`Release` deletes every link directory of the job, after any in-flight `Acquire` of the same job returns** (Responsibilities and import rules, below).
- **A reference is normalised with go-containerregistry's own parser, to its fully qualified name,** so `node:22` and `index.docker.io/library/node:22` are one tag (IMG-1). That's the library's rule, so there is no rule of our own. A reference with both a tag and a digest is its digest.
- **The platform is `linux/<the plugin's GOARCH>`:** a multi-platform index resolves to that image (IMG-2). arm64 isn't refused, but only amd64 is tested in CI and claimed as supported.
- **Credentials come from go-containerregistry's default keychain:** the plugin user's `~/.docker/config.json` (or `$DOCKER_CONFIG`) and its credential helpers (IMG-3).
- **Plain HTTP follows the library's own rule: a registry at a localhost, loopback or RFC 1918 address is tried over HTTPS, then over plain HTTP; every other registry is HTTPS only.** That is a known downgrade on a LAN: someone on the network path to a private-address registry can answer in plain HTTP. It is no code of ours, the in-process test registry needs it, and TLS everywhere would mean a transport of our own overriding the library's. A setting for insecure registries can come with the config file (#8) if anyone asks.
- **An image's raw config is cached as `<cache>/images/<manifest digest hex>.json`, and a tag as `<cache>/tags/<sha256 hex of the normalised reference>`, a JSON object holding the normalised `reference` and the manifest `digest` it resolved to.** A tag file is named by a hash, not the escaped reference, so a long repository path can't outgrow NAME_MAX and no escaping rule is ours; the reference inside it is checked against the one asked for. The digest is the platform image's, not the index's. Both are written like layers: a temp file in `<cache>/tmp/`, fsync, rename into place, fsync of the directory, mode 0600 in a 0700 directory.
- **An image config may be at most 8 MiB; a larger one fails that Start.** A config is held in memory and kept in `<cache>/images/`, and the registry is untrusted. Real configs are a few KiB to a few hundred KiB, so 8 MiB is generous. It is refused by the size its manifest declares, before it is fetched, and the read stops past the limit whatever the registry sends.
- **A tag resolves once and is kept until its config is pruned; there is no tag expiry (#1).** A cached tag whose config is cached is used with no registry request (IMG-6). A tag whose config is gone is looked up again.
- **A digest reference whose config and layers are cached never contacts the registry.** A layer that's missing, evicted for example, is fetched by digest: the image's manifest by its digest (`repository@digest`), never by tag, then only the missing layers' blobs. The manifest must name the cached config's digest and as many layers as the config has diffIDs. An image with only some of its layers gone keeps its config and its tag.
- **So a Start whose image is cached makes no registry request, and writes nothing to the cache but its layers' mtimes and its layer link directory (IMG-7).**
- **`force_pull` looks the tag up on every Start, and a registry error then fails that Start (IMG-6).** A new digest rewrites the tag file; the same digest writes nothing. A Start rewrites a tag file only while it still holds what that Start read before its lookup, so a slow cold Start can't put back a digest that a later lookup, force_pull's for example, replaced: tags never expire, so the older digest would stick. Only the plugin writes tag files, so a lock in the plugin makes the compare and the write one step. A digest reference never changes, so `force_pull` doesn't affect it. It travels in `imagecache.Request.ForcePull`; #8 adds the profile key and passes it, and until then the built-in value, false, applies.
- **An image's config and tag are written only after every one of its layers is linked.** Pruning (#44) deletes configs with no cached layer, so a config written first could be pruned from under a cold Start. A crash or a cancel mid-pull leaves only layers, which eviction can remove.
- **`Image.Env` comes from the cached config.**
- Two Starts that need the same missing layer each convert it until #43 shares pulls.

## Admission: the memory budget

- **Admission is capped by memory, not by VM count.** The plugin config key `max_memory_mib` caps the sum of `memory_mib` over every running VM, across all jobs. Unset or `0` means no limit: an exception to #1's rule that a plugin-level `0` is taken literally, because a budget of 0 MiB admits no job and would only make every job fail at Create. This replaces #1's `max_vms` (QUE-1): the runner's capacity already caps concurrent jobs, a VM count stops meaning anything once a job brings services, and memory is what actually runs out on a host. No CPU or disk cap is added now; they may come later.
- **A job is admitted with all its VMs at once, or it waits.** Waiting jobs are admitted in arrival order (QUE-2). Admission is never VM by VM, because two half-placed jobs could each hold memory the other needs and deadlock.
- **A job that needs more than `max_memory_mib` on its own fails at Create,** naming its need and the cap. Otherwise it would wait forever.
- #10 builds the queue and #8 the config key; this section is the rule they build.

## Failure handling and startup cleanup

- **Each of the state, cache, runtime and listen-socket directories has its own lock: an exclusive, non-blocking `flock` on a `lock` file in that directory** (`<state>/lock`, `<cache>/lock`, `<runtime>/lock`, `<listen>/lock`), held for the plugin's whole life (IMG-16). A single state lock would let two plugins with different state directories share a cache or runtime directory, and then one plugin's startup cleanup would delete the other's live files.
- **The state, cache and runtime directories must be distinct, and none may be inside another.** They are compared as absolute paths with symlinks resolved, before any lock is taken; otherwise startup fails with an error naming both config keys. Startup cleanup deletes `<runtime>/*` without looking: with runtime equal to cache it would wipe the image cache, and with runtime equal to state the kept logs and `bin/`. With state equal to cache, `<state>/jobs/<id>/<n>/` and `<cache>/jobs/<id>/<n>/` collide, so the read-only layer share would expose the VM's scratch disk and logs.
- **The listen-socket directory may be the runtime directory; otherwise the same rule applies to it** (distinct from the other three, not inside one and not containing one), because cleanup of a directory that contains it would delete the listen socket. When it is the runtime directory, it takes no lock of its own, since two `flock`s on one file through two opens in one process block each other, and startup cleanup skips the listen socket.
- **The plugin takes the locks in a fixed order: state, cache, runtime, listen-socket directory.** The first one held by another process stops startup with an error naming that directory.
- **The lock files are opened close-on-exec,** so no job process inherits one and keeps a directory locked after the plugin dies.
- **At startup, after taking every directory lock, the plugin deletes `<state>/jobs/*`, `<state>/bin/*` (stale job binaries and leftover temp files; it then writes the job binary afresh), `<runtime>/*` except `<runtime>/lock` and, when `<listen>` is `<runtime>`, the listen socket, `<cache>/jobs/*` and `<cache>/tmp/*`, without inspecting them.** The locks prove no other plugin uses these directories. The liveness pipe and Pdeathsig mean no old job process outlives the plugin. So everything per-job is leftover. Cleanup never deletes a `lock` file: deleting a held lock file would let a second plugin lock a new file at the same path.
- **A VM child of the previous plugin still dying at that moment can at worst fail one job.** It was reparented to init, not to the new plugin, so the new plugin can't reap it. The disk `flock` is non-blocking (STO-7), so while the dying VM child still holds it, the first job on that disk fails Start with `disk <source> is in use by another process`. A read-write disk is never shared, so nothing is corrupted.
- #11 implements startup cleanup; #5 handles the cache side. Today the plugin deletes only the stale files in `<state>/bin/` at startup.
- **A job's failure costs that job, never the plugin.** Everything that can hang (the job process, its VMs, the agent) is in a child process or behind a call with a context, and ending the job process ends all of it.

## Logging

- **`log/slog` with the text handler (logfmt) on stderr, the same in all three programs.** It is in the standard library and structured.
- **Where stderr goes:** the plugin's is the operator's; the job process's is its job process log file; each VM child's is its VM's log file; the agent's is its VM's console.
- **Secret values have a type that implements `slog.LogValuer` and redacts itself** (`internal/secret`). This covers LOG-2 wherever a value is logged by accident.
- Levels and their configuration are #11's.

## Packages

```
cmd/runner-microvm/                 thin main → plugin
cmd/runner-microvm-job/             thin main → job (dispatches `vm`)
cmd/runner-microvm-agent/           thin main → agent (static)
proto/runnermicrovm/agent/v1/       agent protocol
proto/runnermicrovm/job/v1/         control channel (plugin ↔ job process)
proto/runnermicrovm/vm/v1/          VM child spec (private to the job binary)
internal/gen/runnermicrovm/{agent,job}/v1/      generated, committed
internal/gen/plugin/v1alpha/        the runner's plugin protocol, generated, committed
internal/plugin/                    BackendPlugin server, job table, the two seam interfaces
internal/imagecache/                pulls, conversion, cache, layer link directories, eviction
internal/joblaunch/                 plugin side of a job process: start it, read events, kill it
internal/agentclient/               plugin side of the agent protocol
internal/payload/                   the embedded job binary and agent (`payload` tag) / stub
internal/job/                       the job process
internal/job/internal/jobnet/       the job network
internal/job/internal/vm/           the VM child: libkrun via purego
internal/job/internal/gen/runnermicrovm/vm/v1/  generated, committed
internal/agent/                     the agent: boot mounts, Exec, CopyIn, CopyOut
internal/logging/                   slog setup
internal/secret/                    the self-redacting secret type
```

- **`cmd/<name>/` with thin mains, and everything else under `internal/`, with no `pkg/`.** This is the Go team's layout for multi-binary server repos (go.dev/doc/modules/layout). The project has no public Go API.
- **Names avoid shadowing the standard library** (`logging`, not `log`), and follow the glossary (`imagecache`, not "image store").
- **`config` (#8) and `queue` (#10) are created by their epics,** as `internal/config/` and `internal/queue/`. Both belong to the plugin.

### Responsibilities and import rules

Third-party libraries are free to use except where the boundary test below forbids them. Between our own packages, imports follow this table:

| Package | Responsibility | Our packages it may import | Imported by |
|---|---|---|---|
| `internal/plugin` | `BackendPlugin` server, job table, the two seam interfaces | `imagecache`, `joblaunch`, `agentclient`, `payload`, `config`, `queue`, `logging`, `secret`, the agent and job protocols | `cmd/runner-microvm` |
| `internal/imagecache` | pulls, conversion, the cache, layer link directories, eviction | `logging` | `plugin` |
| `internal/joblaunch` | starts a job process, reads its events, kills it | `logging`, the job protocol | `plugin` |
| `internal/agentclient` | the plugin's side of the agent protocol | `secret`, the agent protocol | `plugin` |
| `internal/payload` | the embedded job binary and agent | none | `plugin` |
| `internal/job` | the job process; dispatches `vm` to the VM child | `job/internal/...`, `logging`, `secret`, the job protocol | `cmd/runner-microvm-job` |
| `internal/job/internal/jobnet` | the job network (gvisor-tap-vsock) | `logging` | `job` only (compiler-enforced) |
| `internal/job/internal/vm` | the VM child: libkrun via purego | `logging`, the VM child spec | `job` only (compiler-enforced) |
| `internal/agent` | the agent: boot mounts, Exec, CopyIn, CopyOut | `logging`, `secret`, the agent protocol | `cmd/runner-microvm-agent` |
| `internal/logging` | slog setup | none | all three programs |
| `internal/secret` | the self-redacting secret type | none | all three programs |

- **The job network and the VM child are nested under `internal/job/internal/`,** so the compiler stops any package outside `internal/job` from importing them.
- **The VM child spec has its own proto package, generated under `internal/job/internal/gen/`.** Only the job binary uses it, so the plugin cannot depend on it.
- **The agent client is its own package (`internal/agentclient`).** Otherwise it drifts into `internal/agent` and pulls host code toward the guest.
- **Code shared by the three programs is only `logging`, `secret` and the generated protocol packages.** Each program's own code stays in its own tree, so a dependency added for one program can't leak into another.
- **Only the plugin's two seams have interfaces:** the image cache and the job launcher, both declared in `internal/plugin`, each with a real and a fake adapter. The data that crosses them is plain: `imagecache.Request` (the job, the VM's index, the image ref and whether to pull again), `imagecache.Image` (digest, image environment, one VM's layer link directory and its layers' names in it) and `joblaunch.Process` (the job process's events and exit). The job launcher reads the job process's stdout to its end whether or not the plugin reads `Events`, and `Events` is buffered for every event the control channel allows for the job spec (one network up and one failed, and one VM started and one VM exited for each VM), so a plugin that stops reading never blocks `Exited` or `Kill`. The seams' contracts on cancel and error:
  - `Launch` that returns an error has killed and reaped every process it started, so no job process is left without a handle to `Kill`.
  - `Exited` closes, and `Kill` returns, only once every process in the job's group has exited and been reaped (Ending a job, above).
  - `Acquire` takes an `imagecache.Request`, not positional arguments, so fields such as per-image registry credentials can be added later without changing callers. It returns one `Image` per VM, whose layer link directory is `<cache>/jobs/<id>/<n>/`, keyed by the VM's index, never its name. Each VM brings its own image once services arrive, so the link directory is the VM's, not the job's. Today only VM 0, the job's container, acquires one, because the plugin protocol passes no VM options for services.
  - `Release` takes only the job, and deletes the layer link directories of all its VMs at once, because a job's VMs are freed together at Remove. It waits for any in-flight `Acquire` for the same job, for any of its VMs, to return before it deletes them, so a cancelled `Acquire` can't finish a link directory after it is deleted and pin its layers against eviction (IMG-14).

  Everything else is concrete: an interface with one implementation is a layer with no payoff.

### Boundary enforcement

- **The compiler enforces the nesting under `internal/job/internal/`.**
- **A Go test (`cmd/deps_test.go`) runs `go list -deps` on each binary and asserts:**
  - the plugin binary's dependencies exclude purego, gvisor, gvisor-tap-vsock, `internal/job`, `internal/agent` and forgejo-runner's module;
  - the job binary's dependencies *include* purego and gvisor-tap-vsock, so a renamed import path can't make the check pass silently, and exclude forgejo-runner's module;
  - the agent binary's dependencies are only from an allow-list: the standard library, grpc and protobuf and the modules they bring in, `internal/agent`, `internal/logging`, `internal/secret` and the agent protocol's generated package.
- **It checks each binary's graph as it ships** (`CGO_ENABLED=0`, and `-tags payload` for the plugin), not as the test process is built, because a file behind a build constraint is in only one of the two.
- **It runs under plain `go test ./...`.** It checks transitive dependencies, which is the property that matters. depguard checks only direct imports, so it isn't used.
- **It lives beside the mains, in `cmd/`,** because it is a test of the binaries, not of any one package.

## Protocols

Three `.proto` files under `proto/`, each in a directory that mirrors its versioned package, beside forgejo-runner's own plugin protocol (`proto/plugin/v1alpha/`, vendored unchanged). The commands are in `docs/development.md` (Protocols and codegen).

| Package | Between | Carries |
|---|---|---|
| `plugin.v1alpha` (the runner's) | runner ↔ plugin (gRPC) | Capabilities, Create, Start, Exec, CopyIn, CopyOut, Remove |
| `runnermicrovm.agent.v1` | plugin ↔ agent (gRPC) | Exec, CopyIn, CopyOut, readiness, the read-write flush; the agent's boot config |
| `runnermicrovm.job.v1` | plugin ↔ job process (control channel) | the job spec in, status events out |
| `runnermicrovm.vm.v1` | job process → VM child | the VM child spec |

- **The `.proto` files define every field #1's behaviours need, not only #4's.** Later epics only add fields.
- **The job spec is job-wide fields plus a list of VMs; the VM child spec is one VM.** The job process starts one VM child for each entry, and status events name the VM by its index (One job process runs all of a job's VMs, above). Today the list always holds one VM, the job's container, because the plugin protocol passes no VM options for services.
- **The agent's boot config is part of the agent protocol's package,** because the agent may import only its own generated package. The plugin builds it, the job spec and the VM child spec carry it unopened, and it reaches the agent as a file in the VM's boot root (The boot root, above), a provisional pick for #6.

## Tests

- **Plain `go test ./...` needs no KVM and no root:**
  - the image cache is tested against go-containerregistry's in-process registry, with real conversion;
  - the agent's Exec and Copy are tested on the host in temporary directories;
  - the job network is tested in-process, with a test peer on the frame socket;
  - the plugin is tested with fakes for its two seams.
- **Tests that boot VMs carry `//go:build kvm`, and `just test-kvm` runs them** with `-tags kvm,payload`, in the `kvm` CI job. That covers the VM child, the job process, the agent's boot mounts, and the protocol test. The tests of the guest's root (`cmd/layers_test.go`: 120 layers, an image without `/proc`, `/sys` or `/dev`, the read-only layer share, a fresh root for each job) build their images in Go on top of `e2e/image` and serve them from go-containerregistry's in-process registry in the test process.
- **The protocol test (`cmd/protocol_test.go`) runs `e2e/workflows/echo.yml` through forgejo-runner's own job executor (`act/runner`), with no Forgejo and no runner daemon.** `act/runner` is the code inside forgejo-runner that calls plugins, through `act/plugin`, so the plugin sees exactly the runner's calls at the pinned version, and a runner bump re-runs the real sequence with nothing recorded to redo.
  - Its config maps the workflow's `microvm` label to `runner-microvm://<the image in e2e/image>`, and the `runner-microvm` plugin to the listen socket of the plugin under test.
  - Its work directory is a fresh, empty git repository, so `act/runner`'s git lookups don't fail noisily.
  - It passes when the executor returns no error and the job's log holds the step's output.
  - It lives beside the mains, in `cmd/`, because it tests the built binary: it builds `runner-microvm` with `-tags payload`, runs it by path with `RUNNER_MICROVM_DIR` set to a temporary directory and a timeout, and stops it with SIGTERM on cleanup. A plugin still running 30 seconds later, or at the timeout, is killed the hard way: first each of its children's process groups, because each job process leads a group of its own, which killing the plugin's group would miss, then the plugin's group. The children are found in `/proc/<pid>/task/*/children` while the plugin still runs, before it is killed and they are reparented.
- **forgejo-runner's module (`code.forgejo.org/forgejo/runner/v13`) is a test-only dependency.** It brings in about 65 indirect modules, the docker client among them, but none reach a shipped binary, `go-licenses` or `govulncheck`: `cmd/deps_test.go` fails if any binary depends on it. Its GPL-3.0-or-later license matches the project's.
- **`cmd/plugin_proto_test.go` checks that the vendored plugin protocol is byte for byte the runner module's copy,** under plain `go test`, so a runner update that changes the protocol fails CI.
- **No test runs its own binary** (`os.Args[0]`, `os.Executable()`). A test that needs a program builds it and runs it by path, with a timeout, and kills its process group on cleanup.
