# SPDX-License-Identifier: GPL-3.0-or-later
#
# libkrun and libkrunfw for the dev shell and CI, at or above the project's
# minimums (nix/lib-minimums.json, docs/development.md).
#
# Each library is overlaid only while nixpkgs' version is older than its
# minimum; once nixpkgs catches up, its own package is used and the override
# below is dead code. Remove the overrides then:
# https://github.com/chrishoage/forgejo-runner-plugin-microvm/issues/26
#
# Renovate does not bump these: a libkrun version change also changes its
# vendored-crate hash, so a bump is a manual change.
final: prev:
let
  inherit (final) lib;
  minimums = lib.importJSON ./lib-minimums.json;
  older = name: lib.versionOlder prev.${name}.version minimums.${name};

  libkrunfw' =
    if older "libkrunfw" then
      prev.libkrunfw.overrideAttrs (
        finalAttrs: _: {
          version = "5.6.4";
          src = final.fetchFromGitHub {
            owner = "libkrun";
            repo = "libkrunfw";
            tag = "v${finalAttrs.version}";
            hash = "sha256-jf8z5dIVIiE5YFElolDxij3k8c8nBeDPhnnCiuhb0YE=";
          };
          # Since 5.6, libkrunfw builds from its own kernel tree, with its
          # patches already applied.
          kernelSrc = final.fetchurl {
            url = "https://github.com/libkrun/linux/archive/refs/tags/v${finalAttrs.version}-6.12.115.tar.gz";
            hash = "sha256-BQ/OqWtgfknxNfJwRyvJU0bBcsnW31+SVq6295cmddU=";
          };
          postPatch = ''
            substituteInPlace Makefile \
              --replace-fail 'curl -L $(KERNEL_REMOTE) -o $(KERNEL_TARBALL)' 'ln -s ${finalAttrs.kernelSrc} $(KERNEL_TARBALL)'
          '';
        }
      )
    else
      prev.libkrunfw;

  # Block and network support are on in either case: without them there are
  # no disks and no job network.
  libkrun' = prev.libkrun.override {
    withBlk = true;
    withNet = true;
  };
in
{
  libkrunfw = libkrunfw';
  libkrun =
    if older "libkrun" then
      libkrun'.overrideAttrs (
        finalAttrs: _: {
          version = "1.19.8";
          src = final.fetchFromGitHub {
            owner = "libkrun";
            repo = "libkrun";
            tag = "v${finalAttrs.version}";
            hash = "sha256-JcMkgtoNlY2cK32ZjlyGCne2gIWXTSqQSEiOadUSd4E=";
          };
          cargoDeps = final.rustPlatform.fetchCargoVendor {
            inherit (finalAttrs) src;
            hash = "sha256-FrHkBeTO1RL/hRT0H6okuGdbGiGNljwUOepBsQvrEug=";
          };
        }
      )
    else
      libkrun';
}
