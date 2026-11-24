# libkrun is bound with purego, not cgo

The VM child binds libkrun with purego, which `dlopen`s it at run time, and every binary builds with `CGO_ENABLED=0`. With cgo, libkrun is linked when the binary loads, so loading it from the directory an operator names (the `lib_dir` key, VM-10) would need `LD_LIBRARY_PATH`, and a missing library would kill the process before it could give a clean error. purego opens the library from the named directory and reports a missing one as an ordinary error, and the libkrun 1.x calls take only ints and strings, which purego passes without C glue. Only the VM child loads libkrun; the plugin and the agent never depend on purego, which a test checks.

## Consequences

- The project has no C toolchain in its build. The dev shell still has a C compiler, because `go test -race` needs cgo.
- A libkrun call whose arguments go beyond ints and strings (structs, callbacks) would need care that cgo would have handled. The 1.x API has none we use.
- A wrong function signature is a run-time crash, not a compile error, so the binding is tested in the `kvm` tier against the real library.
