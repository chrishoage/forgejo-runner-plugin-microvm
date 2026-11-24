# SPDX-License-Identifier: GPL-3.0-or-later
{
  description = "runner-microvm: a microVM backend plugin for forgejo-runner";

  inputs = {
    # nixos-unstable carries current Go and lint tools; a stable channel lags.
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs =
    { self, nixpkgs }:
    let
      # x86_64 only: GitHub's x86 Linux runners provide KVM; arm64 is out of scope.
      system = "x86_64-linux";
      pkgs = import nixpkgs {
        inherit system;
        overlays = [ (import ./nix/libkrun-overlay.nix) ];
      };
      inherit (pkgs) lib;

      # Podman from nixpkgs without the overlay: its runtime, crun, links
      # libkrun, so the overlaid podman would miss the binary cache and
      # rebuild libkrun. crun's libkrun is not the one the plugin loads.
      podman = nixpkgs.legacyPackages.${system}.podman;

      # libkrun and libkrunfw side by side, in one directory, which the
      # tests and the harness give the plugin as its lib_dir.
      libdir = pkgs.symlinkJoin {
        name = "runner-microvm-libs";
        paths = [
          pkgs.libkrun
          pkgs.libkrunfw
        ];
      };

      # Fails evaluation, and so `nix flake check`, when a library resolves
      # below its minimum or libkrun lacks block or network support.
      minimums = lib.importJSON ./nix/lib-minimums.json;
      atLeastMinimum =
        name:
        lib.assertMsg (lib.versionAtLeast pkgs.${name}.version minimums.${name})
          "${name} ${pkgs.${name}.version} is older than the minimum ${minimums.${name}} (nix/lib-minimums.json)";
      libkrunHas =
        flag: lib.assertMsg (lib.elem flag pkgs.libkrun.makeFlags) "libkrun is built without ${flag}";

      # Selected explicitly, so a change to nixpkgs' default `go` can't
      # silently change the Go minor version. A minor bump is a deliberate
      # change of this line and go.mod together.
      go = pkgs.go_1_26;

      # The Go tools are built with the same Go, so every tool in the shell
      # agrees with `go` on the standard library and language version.
      # (nixpkgs builds them with its latest Go, and go-licenses even pins
      # GOROOT to that Go.)
      goTools = {
        golangci-lint = pkgs.golangci-lint.override { buildGo127Module = pkgs.buildGo126Module; };
        go-licenses = pkgs.go-licenses.override {
          buildGoModule = pkgs.buildGo126Module;
          inherit go;
        };
        govulncheck = pkgs.govulncheck.override { buildGoLatestModule = pkgs.buildGo126Module; };
      };
    in
    {
      devShells.${system}.default = pkgs.mkShell {
        packages = [
          go
          pkgs.just
          goTools.golangci-lint
          goTools.go-licenses
          goTools.govulncheck
          libdir
          pkgs.cocogitto
          # git for the commit check, for act/runner's action clones in the
          # protocol test and its git http-backend, and for the harness's
          # push, so none relies on the host's git.
          pkgs.git
          # mkfs.ext4, which formats each VM's scratch disk: a host
          # dependency of the plugin (docs/development.md).
          pkgs.e2fsprogs
          # Protocol codegen and lint, with local plugins: no Buf Schema
          # Registry, no account, no network. They generate code rather than
          # analyse it, so nixpkgs' builds are used as they are.
          pkgs.buf
          pkgs.protoc-gen-go
          pkgs.protoc-gen-go-grpc
          # The local harness (`just e2e`, docs/development.md): the Docker
          # CLI with its compose plugin for `docker compose`, podman for
          # `podman compose` with `docker-compose` on PATH as its provider,
          # curl and jq for Forgejo's API, and iproute2's `ip` for the host's
          # address the jobs reach Forgejo at.
          pkgs.docker-client
          pkgs.docker-compose
          podman
          pkgs.curl
          pkgs.jq
          pkgs.iproute2
          # `go test -race` needs cgo, so the shell carries a C compiler
          # (mkShell's stdenv) even though the binary builds without it.
        ];

        # The flake is the only source of Go: never download a toolchain.
        GOTOOLCHAIN = "local";

        # The directory of libkrun.so.1 and libkrunfw.so.5. Only the tests,
        # the harness and `just check-libs` read it; the plugin reads its
        # lib_dir key.
        RUNNER_MICROVM_LIBDIR = "${libdir}/lib64";
      };

      checks.${system} = {
        devShell = self.devShells.${system}.default;
        libVersions =
          assert atLeastMinimum "libkrun";
          assert atLeastMinimum "libkrunfw";
          assert libkrunHas "BLK=1";
          assert libkrunHas "NET=1";
          pkgs.runCommand "lib-versions" { } ''
            echo "libkrun ${pkgs.libkrun.version}, libkrunfw ${pkgs.libkrunfw.version}" > $out
          '';
      };
    };
}
