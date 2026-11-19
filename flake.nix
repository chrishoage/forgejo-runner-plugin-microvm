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
      pkgs = nixpkgs.legacyPackages.${system};

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
          pkgs.cocogitto
          # `go test -race` needs cgo, so the shell carries a C compiler
          # (mkShell's stdenv) even though the binary builds without it.
        ];

        # The flake is the only source of Go: never download a toolchain.
        GOTOOLCHAIN = "local";
      };

      checks.${system}.devShell = self.devShells.${system}.default;
    };
}
