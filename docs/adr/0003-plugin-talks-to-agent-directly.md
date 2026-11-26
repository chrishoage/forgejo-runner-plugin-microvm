# The plugin talks to the agent directly

The plugin dials each VM's agent over the unix socket libkrun maps to the guest's vsock port, in that VM's runtime directory, `<runtime>/<id>/<n>/`, and the job process relays no streams. Exec output and copied files are long-lived streams; relaying them through the job process would mean a streaming relay to write and maintain, for no gain. The control channel between the plugin and the job process therefore stays minimal: one spec in on stdin and a handful of status events out on stdout, as length-delimited protobuf. It is not gRPC, because one message in and a few out don't need a server. The agent protocol is gRPC, because cancelling a stream cancels the agent's context, which kills the step's process group with no extra code, and gRPC is already a dependency through the runner's protocol.

## Considered options

- **The job process relays every agent call:** the job process would own all of the job, but it would need a streaming gRPC proxy, with its own cancellation and back-pressure bugs.
- **gRPC on the control channel too:** a server for one message in and a few out.

## Consequences

- A VM's agent is up when it answers Ping, and the job's container is then ready; a service's VM is ready once the plugin's `StartService` to its agent succeeds. The plugin learns both by calling the agent, not from a job process event. Start dials every VM's agent, the job's container's and each service's, and keeps only the job's container's afterwards, since Exec and Copy name only the job.
- Each VM's agent socket lives in `<runtime>/<id>/<n>/`, where the plugin can reach it, while the job process owns the VM behind it. The plugin's Exec and Copy go to the job's container's agent, because the plugin protocol's Exec and Copy name only the job, not a VM.
- The epic's "job process ↔ agent protocol" is the plugin ↔ agent protocol.
