# internal/provider

The compute contract and its six backends: docker, firecracker, tart, ec2, codebuild, and simulated (for billet's own test harness, refused in a config). Load `billet-providers-local` or `billet-providers-aws` before changing a backend, and `billet-add-provider` to add one.

- A provider is below the scheduler: the `provider` depguard rule refuses `server`, `node` and `cmd`, and `store` is its sibling, never its dependency.
- `List` errors rather than answering short, because reconciliation frees every lease absent from it.
- `Accepts(trust)` is asked before anything expensive, and untrusted work needs a real boundary (`billet-security`).
- Never quote the guest in an error.
- A fake proves the shape of a request, never the values a real host or service refuses; each backend's `real*_test.go` is the evidence.

Gates: `make check`; `make cross` before anything touching a build tag.
