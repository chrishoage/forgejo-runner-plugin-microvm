# runner-microvm

`runner-microvm` is a backend plugin for [forgejo-runner](https://code.forgejo.org/forgejo/runner) that runs each job in microVMs of its own. It is in early development and does nothing useful yet.

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
