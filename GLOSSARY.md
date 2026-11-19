# runner-microvm

A backend plugin for forgejo-runner that runs each job's container image as its own microVM.

## Jobs and the runner

**Runner**:
forgejo-runner, which takes jobs from Forgejo and hands each one to the plugin.

**Plugin**:
`runner-microvm`'s long-lived process: it serves the runner and owns what jobs share, such as the image cache and VM slots.
_Avoid_: server, daemon, backend

**Job**:
One workflow job as the plugin sees it, from Create to Remove.
_Avoid_: environment, task, run

**Environment id**:
The runner protocol's name for a job's identifier. Use it only when talking about the protocol; everywhere else say job.

**Label**:
A runner label routed to the plugin, naming an image and optionally a profile.

**Profile**:
A named VM shape (CPUs, memory, scratch size, mounts, disks) that an operator defines and a label selects.
_Avoid_: flavour, template, size

## Per-job processes

**Job process**:
The host process that owns everything belonging to one job: its job network, its VM child, its disk locks and its files. Ending it ends the job.
_Avoid_: job runner, worker, shim

**Job binary**:
The program the job process runs, embedded in `runner-microvm` and written out by the plugin.

**VM child**:
The process the job process starts to run the job's VM; the only process that loads libkrun.
_Avoid_: VMM process, krun process

**Control channel**:
The link between the plugin and a job process: a spec goes in, status events come out.

**Liveness pipe**:
A pipe held from the plugin down to every job process and VM child, whose closing tells them the plugin is gone.

**Job network**:
A job's private network, run inside its job process, that only its VM can use.

## Inside the VM

**VM**:
The microVM a job runs in, booted fresh from the job's image and discarded at Remove.
_Avoid_: sandbox, container, guest (except for what runs inside it)

**Agent**:
The program injected into every VM that runs the job's steps and copies files in and out.
_Avoid_: guest daemon, init

**Agent protocol**:
The plugin's own protocol between the plugin and the agent.

**Scratch disk**:
A fresh per-job disk that holds every change the job makes to its root filesystem.

## Images and storage

**Image cache**:
The plugin's on-host store of image layers, shared by all jobs.
_Avoid_: image store, registry

**Layer link directory**:
The per-job view of the image cache holding exactly the job's own layers.

**Mount**:
A host directory a profile shares into the VM.

**Disk**:
A host file a profile attaches to the VM as a block device.
_Avoid_: volume

**VM slot**:
A place under the plugin's limit on VMs running at once.

## Process

**Shortcut**:
A deliberately naive piece of code, marked with the issue that replaces it.
