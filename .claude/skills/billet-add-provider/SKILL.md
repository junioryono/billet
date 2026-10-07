---
name: billet-add-provider
description: "Load when adding a compute backend under internal/provider: the contract it must keep, the closed set of provider kinds in internal/config, RunsOnHost and what capacity charges, the trust gate, the depguard layering, where the CLI builds it, and the real test that has to back the fake."
---

# Adding a compute backend

A checklist. The contract's rules are in `billet-providers-local` (`references/contract.md`), the AWS rules in `billet-providers-aws`, and the trust rules in `billet-security`.

1. **The kind.** Add it to the closed set of `config.ProviderKind` in `internal/config`, with its validation (which blocks it requires or refuses). Decide `ProviderKind.RunsOnHost()`: a backend whose work runs somewhere else is charged the shape it buys, never the tier's request, and the predicate is what makes that true (`billet-capacity`, `references/leases.md`).
2. **The contract.** Implement `internal/provider`'s interface:
   - `Accepts(trust)` is answered before anything expensive;
   - `List` errors rather than answering short, because reconciliation frees every lease absent from it;
   - one process writes the launch verdict from the closed vocabulary;
   - a cancelled command is not a returned one;
   - nothing a guest wrote is quoted into an error.
3. **Trust.** Untrusted work needs a real boundary and a network of its own; say what this backend can and cannot isolate, and refuse in `Accepts` what it cannot.
4. **The layering.** The package lives under `internal/provider/<kind>` and imports neither `server`, `node` nor `cmd` (the `provider` depguard rule). Storage it needs (Firecracker's root disks, for example) arrives through an interface it is handed, never by importing a store.
5. **The assembly.** The CLI builds a provider in `app.NewProvider` (`internal/app/node.go`), which `app.OpenNode` calls only once the node holds its deployment lock. A backend whose work runs somewhere else has an ordered shape catalogue, and two separate switches on the provider kind read it: `remoteShapes` in `cmd/billet/main.go` is what the node registers (a kind it misses registers no shapes, and the allocator refuses a remote provider without them), and `Config.localRemoteShapes` in `internal/config` is what local tier-fit validation checks (a kind it misses passes validation without checking fit). Prove registration through the CLI's own construction path, and separately that configuration refuses an eligible tier no declared shape fits. Add the kind to the provider lists in `ListRemoteCostNodes` and `ListOutstandingRemoteShapes` (`internal/state/queries/nodes.sql`) too, or cost reporting never sees it, then run `make sqlc`; `TestTheRemoteProviderListMatchesTheQueries` holds the lists to the kinds. `nodeLaunchConcurrency` in the same file decides whether its launches run beside each other.
6. **The evidence.** Unit tests against a fake prove the shape of a request, never what a real host or service refuses. Add a `real<kind>_test.go` gated on its resource, which skips without it and never spends money by accident (`billet-testing`). There is no shared contract suite, so cover each contract point in the backend's own tests.
7. **The records.** Add the kind to `billet-providers-local` or `billet-providers-aws`, the configuration reference, and `CLAUDE.md`'s map.
