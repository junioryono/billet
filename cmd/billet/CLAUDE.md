# cmd/billet

The binary: `main`, the `server` and `node` roles' commands, and the two commands that compose several command families, `billet status` and `billet acceptance`. Every other command is a family under `internal/ops` (`cache`, `images`, `fleetops`, `setup`, `host`) that `main.go`'s command tree dispatches to. It is the only package of the binary allowed `os.Exit`, and its `main` is the one place that names the process's streams and environment: every command writes to the `cli.Env` it is handed and reads its environment through it, and forbidigo holds that (tests and the standalone tools under `scripts/` have their own exemptions in `.golangci.yml`). A test hands a command `processEnv()`, or an env with buffers to read what it printed.

- Both roles' assembly is `internal/app`'s: the server's `OpenControlPlane`, `BecomeController` and the `Controller`'s steps, which `runServer` only invokes, and the node's `OpenNode` and `Node.Run`, which `cmdNode` invokes after its flags, enrollment, the probe and the drain handler. Load `billet-assembly` before changing either.
- A test that drives a family's code through `status` or `acceptance` stays here; one that drives the family alone moves with it. Test helpers shared with a family are copied into each package (`*_helpers_test.go`), never exported from a test file.
- Coverage ignores `main.go` (`.codecov.yml`), so new logic goes in another file, and usually in a family.

Gates: `make check`; `make cross` before anything touching a build tag.
