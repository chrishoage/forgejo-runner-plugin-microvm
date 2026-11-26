# NixOS

How to run `runner-microvm` on NixOS with this repository's flake: its package and its module, `services.runner-microvm`, beside nixpkgs' `services.forgejo-runner`. It replaces most of the [operator guide](operating.md): the module creates the user, the directories, the config file and the service, and the package brings its own libkrun, libkrunfw and glibc. The config file's keys are in [`configuration.md`](configuration.md); the words are the glossary's ([`GLOSSARY.md`](../GLOSSARY.md)).

## What the host needs

- **x86_64 NixOS with KVM.** `/dev/kvm` must be read-write for a group the plugin's user joins: `kvm` by default, which NixOS gives `/dev/kvm`. Where `/dev/kvm` arrives with another group, set `services.runner-microvm.kvmGroup` to it.
- Everything else of the operator guide's [list](operating.md#what-the-host-needs) holds, except the loader and the libraries, which the package brings, and `mkfs.ext4`, which the module puts on the unit's `PATH`.

## Adding the flake

Pin a release tag: the package's version is the tagged release's, and `main` between releases may not build.

```nix
{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    runner-microvm.url = "github:chrishoage/forgejo-runner-plugin-microvm/v0.1.0";
  };

  outputs =
    { nixpkgs, runner-microvm, ... }:
    {
      nixosConfigurations.ci = nixpkgs.lib.nixosSystem {
        system = "x86_64-linux";
        modules = [
          runner-microvm.nixosModules.default
          ./configuration.nix
        ];
      };
    };
}
```

- **The package is built from the flake's own pinned nixpkgs,** with the Go version and the libkrun and libkrunfw it was tested with. `runner-microvm.inputs.nixpkgs.follows = "nixpkgs"` builds it from yours instead, which works only while your nixpkgs has the same Go minor (`go_1_26`) and libraries at or above the minimums.
- **`packages.x86_64-linux.default` is the package alone,** for a host that runs the plugin some other way.

## The plugin

```nix
{
  services.runner-microvm = {
    enable = true;
    settings = {
      max_memory_mib = 8192;
      profiles.default = {
        cpus = 2;
        memory_mib = 2048;
      };
    };
  };
}
```

- **`settings` is the config file,** written to `/etc/runner-microvm/config.yaml`, with any key [`configuration.md`](configuration.md) lists. The module sets `state_dir`, `cache_dir`, `runtime_dir` and `listen` to the built-in values, the directories its unit creates, and they can't be set. It sets `lib_dir` to the package's libraries; set it only to load others.
- **A change to `profiles` reloads the plugin on `nixos-rebuild switch`; a change to any other key, or to the package, restarts it,** since only profiles reload ([Reload](configuration.md#reload)). A restart drains: `switch` waits until the running jobs finish, for at most `drainTimeout`.
- **`drainTimeout`** (default `3h`, forgejo-runner's default job timeout) is how long a stop waits for the drain before it kills the plugin and its jobs.
- **To have a switch kill running jobs rather than wait for them, shorten `drainTimeout`,** for example to `"30s"`. Past it, systemd kills every process of the unit (`KillMode=mixed`), the job processes and their VMs included; the killed jobs fail, and the plugin's next start deletes what they left. Use `"1s"` for no wait, never `"0"`, which systemd reads as no timeout at all.
- **To leave the plugin running across switches instead, set `systemd.services.runner-microvm.restartIfChanged = false;`.** A new package or plugin-level setting then takes effect at your next `systemctl restart runner-microvm`, which drains, or at boot.
- **The plugin runs as the system user `runner-microvm`, whose home is `/var/lib/runner-microvm`:** registry credentials go in `/var/lib/runner-microvm/.docker/config.json` ([Registries](operating.md#registries)). A credential helper goes on the unit's `PATH`: `systemd.services.runner-microvm.path = [ pkgs.docker-credential-helpers ];`.
- **The unit sees the file system read-only, except its own directories:** add a read-write mount's or disk's source with `systemd.services.runner-microvm.serviceConfig.ReadWritePaths`. `/home` and `/root` are hidden from it (`ProtectHome`), so no mount or disk source can be there.
- **Check a file before a switch** with the package's `runner-microvm check`, as the plugin's user: `sudo -u runner-microvm runner-microvm check`.

## The runner

nixpkgs' `services.forgejo-runner` runs forgejo-runner; three parts of its settings, and one line for its unit, point it at the plugin:

```nix
{ config, ... }:
{
  services.forgejo-runner.instances.ci = {
    enable = true;
    settings = {
      server.connections.default = {
        url = "https://forgejo.example.com/";
        uuid = "…";
      };
      runner.labels = [ "microvm:runner-microvm://docker.io/library/alpine:3.24" ];
      plugins.runner-microvm.address = config.services.runner-microvm.address;
      # Or cache.host: an address of this host the VMs can reach, never loopback.
      cache.enabled = false;
    };
    secrets.server.connections.default.token_url = "/run/keys/forgejo-runner-token";
  };

  # The listen socket is mode 0660, in a directory only its group may enter.
  systemd.services.forgejo-runner-ci.serviceConfig.SupplementaryGroups = [
    config.services.runner-microvm.group
  ];
}
```

- **The labels and the cache** follow [Configuring the runner](operating.md#configuring-the-runner); the plugins key, `runner-microvm` here, is the labels' scheme.
- **The runner's unit runs under a dynamic user,** which can still hold a static group: `SupplementaryGroups` adds the socket's group, `runner-microvm`, to it. The unit is `forgejo-runner-<instance>`.
- **Run the forgejo-runner version the release's notes name** (`services.forgejo-runner.package`): the plugin protocol is `v1alpha` and can change between runner releases.

## What the module doesn't do

- **Egress filtering,** as on any host ([Network](operating.md#network)).
- **A memory limit for the plugin's own process;** `max_memory_mib` caps the VMs' memory.
- **Hardening that a user namespace can't apply:** the unit restricts no devices, kernel tunables, namespaces or system calls, so it also runs in a user-namespaced container that passes `/dev/kvm` in. Add more through `systemd.services.runner-microvm.serviceConfig` where your host allows it, and run a job afterwards: libkrun maps guest memory and runs a VMM in the VM child, which a system call filter or `MemoryDenyWriteExecute` can break.
