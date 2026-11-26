# SPDX-License-Identifier: GPL-3.0-or-later
#
# services.runner-microvm: the plugin as a system service (docs/nixos.md).
# The flake's nixosModules.default imports this file and sets `package`.
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.runner-microvm;
  yaml = pkgs.formats.yaml { };

  name = "runner-microvm";

  # The plugin's built-in directories, which the unit's StateDirectory,
  # CacheDirectory and RuntimeDirectory create (docs/configuration.md).
  dirs = {
    state_dir = "/var/lib/${name}";
    cache_dir = "/var/cache/${name}";
    runtime_dir = "/run/${name}/jobs";
    listen = "unix:///run/${name}/${name}.sock";
  };

  configFile = yaml.generate "config.yaml" cfg.settings;

  # Only profiles reload; every plugin-level key takes effect at a restart
  # (docs/configuration.md, Reload).
  pluginLevel = builtins.toJSON (removeAttrs cfg.settings [ "profiles" ]);
  profiles = builtins.toJSON (cfg.settings.profiles or { });

  dirOption =
    key:
    lib.mkOption {
      type = lib.types.str;
      readOnly = true;
      default = dirs.${key};
      description = ''
        Set by the module, to the directory the unit creates. Read only.
      '';
    };
in
{
  options.services.runner-microvm = {
    enable = lib.mkEnableOption "runner-microvm, a microVM backend plugin for forgejo-runner";

    package = lib.mkOption {
      type = lib.types.package;
      defaultText = lib.literalMD "the flake's `packages.x86_64-linux.default`";
      description = "The runner-microvm package.";
    };

    settings = lib.mkOption {
      default = { };
      description = ''
        The plugin's config file, written to {file}`/etc/runner-microvm/config.yaml`.
        Every key is in the configuration reference, `docs/configuration.md`.
        The module sets the directories and `listen`, and `lib_dir` to the
        package's libraries.
      '';
      example = lib.literalExpression ''
        {
          max_memory_mib = 8192;
          profiles.default = {
            cpus = 2;
            memory_mib = 2048;
          };
        }
      '';
      type = lib.types.submodule {
        freeformType = yaml.type;
        options = {
          state_dir = dirOption "state_dir";
          cache_dir = dirOption "cache_dir";
          runtime_dir = dirOption "runtime_dir";
          listen = dirOption "listen";
          lib_dir = lib.mkOption {
            type = lib.types.str;
            default = cfg.package.libDir;
            defaultText = lib.literalExpression "config.services.runner-microvm.package.libDir";
            description = ''
              The directory libkrun and libkrunfw are loaded from: by default the
              ones the package was built and tested with.
            '';
          };
        };
      };
    };

    kvmGroup = lib.mkOption {
      type = lib.types.str;
      default = "kvm";
      description = ''
        The group that may open {file}`/dev/kvm` read-write, which the plugin's
        user joins. Where a host passes {file}`/dev/kvm` in with another group,
        name that group here.
      '';
    };

    drainTimeout = lib.mkOption {
      type = lib.types.str;
      default = "3h";
      description = ''
        How long a stop waits for the plugin to drain, its running jobs
        finishing, before it is killed: the unit's `TimeoutStopSec`. Make it
        as long as the jobs you want to finish; forgejo-runner's job timeout
        is 3h by default. Past it, every process of the unit is killed, jobs
        and VMs included, and the next start cleans up after them. `"0"`
        means no timeout to systemd, not none: for no wait, use `"1s"`.
      '';
    };

    address = lib.mkOption {
      type = lib.types.str;
      readOnly = true;
      default = dirs.listen;
      description = ''
        The plugin's listen socket, for forgejo-runner's
        `plugins.<key>.address`. Read only.
      '';
    };

    group = lib.mkOption {
      type = lib.types.str;
      readOnly = true;
      default = name;
      description = ''
        The socket's group: the runner's unit needs it in
        `SupplementaryGroups`. Read only.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    users.users.${name} = {
      isSystemUser = true;
      group = name;
      # Where ~/.docker/config.json, the registry credentials, is read from.
      home = dirs.state_dir;
    };
    users.groups.${name} = { };

    environment.etc."runner-microvm/config.yaml".source = configFile;

    # For `runner-microvm check`, run as the plugin's user.
    environment.systemPackages = [ cfg.package ];

    systemd.services.${name} = {
      description = "runner-microvm, a microVM backend plugin for forgejo-runner";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];

      # mkfs.ext4 formats each VM's scratch disk.
      path = [ pkgs.e2fsprogs ];

      restartTriggers = [ pluginLevel ];
      reloadTriggers = [ profiles ];

      serviceConfig = {
        ExecStart = "${lib.getExe cfg.package} serve";
        ExecReload = "${pkgs.coreutils}/bin/kill -HUP $MAINPID";
        Restart = "on-failure";

        User = name;
        Group = name;
        SupplementaryGroups = [ cfg.kvmGroup ];

        StateDirectory = name;
        StateDirectoryMode = "0700";
        CacheDirectory = name;
        CacheDirectoryMode = "0700";
        # The socket's directory: the runner's user enters it through the
        # group.
        RuntimeDirectory = name;
        RuntimeDirectoryMode = "0750";

        # SIGTERM drains; it goes to the plugin alone, and the jobs it serves
        # end with it.
        KillMode = "mixed";
        TimeoutStopSec = cfg.drainTimeout;

        # Only what works inside a user namespace: no device, kernel tunable,
        # namespace or system call restrictions, which an unprivileged
        # container may be unable to apply (docs/architecture.md, The NixOS
        # module's unit).
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
      };
    };
  };
}
