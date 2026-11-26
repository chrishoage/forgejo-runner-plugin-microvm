# A job process per job, with libkrun in a VM child

Each job runs in its own job process, which owns the job's network, disk locks and files, and libkrun runs in VM children that the job process starts, one for each of the job's VMs: the job binary run again with a `vm` subcommand, from the path the plugin passes in the job spec. libkrun can't share a process with code that must outlive the VM: `krun_start_enter` never returns, and libkrun ends the whole process when the guest stops. In a child, libkrun's Rust threads never share an address space with the Go code that owns the job network and the disk locks, and the job process survives its VMs to report why each one stopped. Every child holds the liveness pipe and also gets `Pdeathsig: SIGKILL`, forked from a dedicated locked OS thread, so it dies with its parent even while its Go code is stuck in libkrun. A failed Start kills the job process's group at once, dropping its VMs, network and disk locks without waiting for Remove.

## Considered options

- **libkrun in the plugin process:** one hung or crashed VM would take every job with it, and the plugin can't outlive `krun_start_enter`.
- **libkrun in the job process itself:** the job network and disk locks would die with the VM, and nothing would be left to report why it stopped.

## Consequences

- Each job has host processes of its own beside the shared plugin: the job process and one VM child for each of its VMs. A job has one VM for its container and one for each service, so a job without services has two.
- The job binary has two entry points, the job process and `vm`. Nothing ever execs `os.Executable()`: the binary's path always comes from the spec, so a test binary can't recurse.
- Each VM child's inputs are inherited: its spec on stdin, the liveness pipe on fd 3, its VM's end of the job network socket on fd 4 and each of its VM's read-write disks' locked file from fd 5. The disk lock lasts as long as any process that can write the disk.
- A VM child is not the plugin's child, so the plugin is a child subreaper (`PR_SET_CHILD_SUBREAPER`): it inherits each VM child whose job process dies and reaps it. A job's disks are freed only once its whole process group, VM children included, has exited and been reaped, so the next job on a disk never finds it still locked by a dying VM child.
