# runner-microvm

A backend plugin for forgejo-runner that runs each job's VMs as microVMs: the job's container and, once the plugin protocol can configure them, its services.

## Jobs and the runner

**Runner**:
forgejo-runner, which takes jobs from Forgejo and hands each one to the plugin.

**Plugin protocol**:
forgejo-runner's `plugin.v1alpha` gRPC protocol, through which the runner hands the plugin a job. What it doesn't pass is a limit of today, not a design rule.
_Avoid_: plugin API

**Plugin**:
`runner-microvm`'s long-lived process: it serves the runner and owns what jobs share, such as the image cache and the memory budget.
_Avoid_: server, daemon, backend

**Job**:
One workflow job as the plugin sees it, from Create to Remove.
_Avoid_: environment, task, run

**Environment id**:
The runner protocol's name for a job's identifier. Use it only when talking about the protocol; everywhere else say job.

**Backstop**:
The plugin's own removal of a job the runner has not removed by the job's environment timeout plus 30 s.
_Avoid_: reaper (reaping is of processes), expiry

**Label**:
A runner label routed to the plugin, naming an image and optionally a profile. Today it is the only way a job chooses its VM, because the plugin protocol passes no per-job VM options.

**Profile**:
A named VM shape (CPUs, memory, scratch size, mounts, disks) that an operator defines. Today a label selects it, and it is the only source of a VM's shape and storage, applied only to the job's container, because the plugin protocol passes no per-job or per-service VM options.
_Avoid_: flavour, template, size

**Memory budget**:
The plugin's cap on the memory of every running VM, across all jobs (`max_memory_mib`). A job is admitted only when all its VMs fit.
_Avoid_: VM slot

**Config file**:
The one YAML file in which the operator sets the plugin's settings and its profiles.
_Avoid_: settings file, plugin config (the runner's own config file is "the runner config")

**Built-in value**:
The value a setting takes when the config file leaves it out, or when there is no config file.
_Avoid_: default (which is the name of a profile)

**Reload**:
Reading the config file again while the plugin runs, so later jobs get its new profiles.

**Drain**:
How the plugin stops on SIGTERM or SIGINT: it takes no new jobs, serves the jobs it has until each is removed, then exits. A second signal ends it at once.
_Avoid_: graceful shutdown, stop

**Startup cleanup**:
Deleting, after the plugin takes its directory locks, every per-job file an earlier plugin process left.
_Avoid_: garbage collection, recovery

## Per-job processes

**Job process**:
The host process that owns everything belonging to one job: its job network, its VM children, its disk locks and its files. Ending it ends the job.
_Avoid_: job runner, worker, shim

**Job logs**:
A job's process log, and each of its VMs' log and console, on the host. Deleted at Remove unless the operator keeps them; then they are kept logs.
_Avoid_: job output (what steps print, which goes to the runner)

**Job binary**:
The program the job process runs, embedded in `runner-microvm` and written out by the plugin.

**VM child**:
A process the job process starts to run one of the job's VMs, one per VM; the only process that loads libkrun.
_Avoid_: VMM process, krun process

**Control channel**:
The link between the plugin and a job process: a spec goes in, status events come out.

**Liveness pipe**:
A pipe held from the plugin down to every job process and VM child, whose closing tells them the plugin is gone.

**Job network**:
A job's private network, run inside its job process, that only its VMs can use.

**Gateway**:
The job network's own address: every VM's default route and its only name server. Connections through it leave the host as the job process's own.
_Avoid_: router, host (the host is the machine the plugin runs on)

## Inside the VM

**VM**:
A microVM of a job, booted fresh from its image and discarded at Remove. Today each job has one, the job's container, because the plugin protocol passes no VM options for services. A VM is identified by its index in the job spec, 0 being the job's container; its name is only its DNS name on the job network and its name in log lines, never a path or a cache key.
_Avoid_: sandbox, container (except in "the job's container"), guest (except for what runs inside it)

**Job's container**:
The VM a job's steps run in, booted from the job's image; always the first VM in the job spec, index 0. Once services return, each service is another VM of the same job, on the same job network.

**Agent**:
The program injected into every VM that prepares it at boot, runs commands and copies files in and out. The job's steps run through the job's container's agent.
_Avoid_: guest daemon, init

**Agent protocol**:
The plugin's own protocol between the plugin and the agent.

**Boot config**:
What the plugin tells a VM's agent at boot: how to build the VM's root, its mounts, disks and network, and the image's defaults for Exec.
_Avoid_: agent config

**Scratch disk**:
A fresh disk for each VM that holds every change the job makes to that VM's root filesystem.

**Scratch path**:
A guest directory that a profile puts straight on the scratch disk instead of the overlay. It starts empty at every boot and hides what the image has there.

## Images and storage

**Image cache**:
The plugin's on-host store of image layers, shared by all jobs.
_Avoid_: image store, registry

**Layer**:
One image layer as the image cache stores it, converted once and shared by every image and VM that uses it.
_Avoid_: blob

**Pull**:
Fetching an image's missing layers and converting them into the image cache.
_Avoid_: download

**Tag file**:
The image cache's record of what a tag, or a multi-platform index's digest, resolved to: the normalised reference and the platform image's manifest digest. It is kept until the image's config is evicted or pruned; `force_pull` looks the tag up again.
_Avoid_: tag cache, index

**Eviction**:
Deleting least-recently-used layers no VM is using, and image configs no job is using, until the image cache fits its cap. A config is in use from its Start's lookup to its job's Release, like its layers.
_Avoid_: GC, prune. Pruning means cleaning up configs and tags only.

**Layer link directory**:
A VM's view of the image cache, holding exactly the layers of that VM's image.

**Boot root**:
The small read-only directory a VM boots from, holding the agent and its boot config, and the bottom layer of the VM's root filesystem.

**Mount**:
A host directory shared into a VM. Today only the label's profile names mounts, for the job's container, because the plugin protocol passes no workflow mounts or volumes (STO-11, a limit of today's plugin protocol).

**Disk**:
A host file attached to a VM as a block device. Today only the label's profile names disks, for the job's container, because the plugin protocol passes no workflow mounts or volumes (STO-11, a limit of today's plugin protocol).
_Avoid_: volume, which is the workflow's `container.volumes` key; the plugin protocol doesn't pass it today.

**Disk lock**:
A job process's hold on a disk, which the VM child it is attached to shares, until that VM child has exited: exclusive for a read-write disk, shared for a read-only one. Any number of VMs can read a disk at once, but a writer excludes every other user, readers included.
_Avoid_: lease, reservation

**Flush**:
Writing everything a VM holds for its read-write mounts and disks through to the host, and leaving each read-write disk clean, just before its job ends normally. A VM that is killed is never flushed, so its last writes may be lost.
_Avoid_: sync (the system call it uses), shutdown

## Process

**Harness**:
The local compose stack, a real Forgejo and a real forgejo-runner, that runs a workflow end to end on a developer's machine (`just e2e`). Never run in CI.
_Avoid_: test environment, e2e stack

**Protocol test**:
A test that runs workflows, every one in `e2e/workflows/` that needs no harness, through forgejo-runner's own job executor (`act/runner`) against the real plugin booting real VMs, with no Forgejo; it runs in KVM CI (`just test-kvm`).
_Avoid_: integration test, e2e test

**Shortcut**:
A deliberately naive piece of code, marked with the issue that replaces it.
