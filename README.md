# runner-microvm

`runner-microvm` is a backend plugin for [forgejo-runner](https://code.forgejo.org/forgejo/runner) that runs each job in microVMs of its own. It is in early development.

## Install

A release is one x86_64 Linux binary, `runner-microvm-<version>-linux-amd64`, with `SHA256SUMS` and a build provenance attestation, on this repository's [releases](https://github.com/chrishoage/forgejo-runner-plugin-microvm/releases). The host also needs KVM, libkrun and libkrunfw.

- [`docs/operating.md`](docs/operating.md), the operator guide: what the host needs, installing and verifying the binary, running it beside forgejo-runner, and what it doesn't do.
- [`docs/configuration.md`](docs/configuration.md): every key of the plugin's config file, and the runner labels that route jobs to it.

## Development

Everything runs inside the project's Nix dev shell:

```sh
nix develop          # enter the dev shell (or use direnv: `.envrc` is `use flake`)
just build           # build bin/runner-microvm
just check           # everything a PR must pass
```

See [`docs/development.md`](docs/development.md).

## License

GPL-3.0-or-later. See [`LICENSE`](LICENSE) and [ADR 0001](docs/adr/0001-gpl-3-or-later-and-dependency-allow-list.md).
