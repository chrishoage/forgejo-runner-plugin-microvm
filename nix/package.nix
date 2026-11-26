# SPDX-License-Identifier: GPL-3.0-or-later
#
# runner-microvm, built by the justfile's own `plugin` recipe, so this
# derivation adds no second build definition (docs/development.md, Building
# and the payload). The job binary keeps Go's default loader, the Nix store's
# glibc, which the libraries in `libDir` are linked against.
{
  lib,
  buildGoModule,
  just,
  libdir,
}:

buildGoModule (finalAttrs: {
  pname = "runner-microvm";
  version = lib.fileContents ../VERSION;

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../justfile
      ../e2e/mod.just
      ../cmd
      ../internal
    ];
  };

  # Refreshed by `just vendor-hash` whenever go.mod or go.sum change
  # (docs/conventions.md, Renovate).
  vendorHash = "sha256-FU2U1jORWOLhNnDV4WoFXiHElrl9OOwHNvReApFHQMA=";

  nativeBuildInputs = [ just ];

  env = {
    CGO_ENABLED = "0";
    GOTOOLCHAIN = "local";
  };

  buildPhase = ''
    runHook preBuild
    just plugin "$out/bin/runner-microvm" \
      '-X github.com/chrishoage/forgejo-runner-plugin-microvm/internal/plugin.version=${finalAttrs.version}'
    runHook postBuild
  '';

  # The tests need the dev shell's tools, and the KVM ones /dev/kvm: `just
  # test` and `just test-kvm` run them.
  doCheck = false;

  installPhase = ''
    runHook preInstall
    runHook postInstall
  '';

  passthru = {
    # The directory of libkrun.so.1 and libkrunfw.so.5 this build is meant
    # for: the NixOS module's built-in lib_dir.
    libDir = "${libdir}/lib64";
  };

  meta = {
    description = "A microVM backend plugin for forgejo-runner";
    homepage = "https://github.com/chrishoage/forgejo-runner-plugin-microvm";
    license = lib.licenses.gpl3Plus;
    mainProgram = "runner-microvm";
    platforms = [ "x86_64-linux" ];
  };
})
