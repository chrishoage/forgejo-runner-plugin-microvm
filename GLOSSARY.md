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

**Label**:
A runner label routed to the plugin, naming an image and optionally a profile. Today it is the only way a job chooses its VM, because the plugin protocol passes no per-job VM options.

**Profile**:
A named VM shape (CPUs, memory, scratch size, mounts, disks) that an operator defines. Today a label selects it, and it is the only source of a VM's shape and storage, applied only to the job's container, because the plugin protocol passes no per-job or per-service VM options.
_Avoid_: flavour, template, size

**Memory budget**:
The plugin's cap on the memory of every running VM, across all jobs (`max_memory_mib`). A job is admitted only when all its VMs fit.
_Avoid_: VM slot

## Per-job processes

**Job process**:
The host process that owns everything belonging to one job: its job network, its VM children, its disk locks and its files. Ending it ends the job.
_Avoid_: job runner, worker, shim

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

**Scratch disk**:
A fresh disk for each VM that holds every change the job makes to that VM's root filesystem.

## Images and storage

**Image cache**:
The plugin's on-host store of image layers, shared by all jobs.
_Avoid_: image store, registry

**Layer link directory**:
A VM's view of the image cache, holding exactly the layers of that VM's image.

**Mount**:
A host directory shared into a VM. Today only the label's profile names mounts, for the job's container, because the plugin protocol passes no workflow mounts or volumes (STO-11, a limit of today's plugin protocol).

**Disk**:
A host file attached to a VM as a block device. Today only the label's profile names disks, for the job's container, because the plugin protocol passes no workflow mounts or volumes (STO-11, a limit of today's plugin protocol).
_Avoid_: volume, which is the workflow's `container.volumes` key; the plugin protocol doesn't pass it today.

## Process

**Shortcut**:
A deliberately naive piece of code, marked with the issue that replaces it.
