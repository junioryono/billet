---
name: billet-config
description: "Load when touching internal/config or internal/initconfig, billet.example.yaml, deploy/billet.yaml, `billet init`, `billet github-app create --config`, or any validation of tiers, nodes or sites; or when you need which role and which provider requires or refuses a block of billet.yaml."
---

# Configuration

## What this area is

`internal/config` parses and validates `billet.yaml` (`Config`, `Load`, `Parse`, the `Check*` validators). It imports nothing else of billet's, enforced by depguard, so a rule that `alloc` also needs is exported from `config` and called from both. `billet.example.yaml` is the annotated reference and `deploy/billet.yaml` the packaged template; both are parsed by the config test suite, so a renamed key breaks the build rather than an install. `internal/initconfig` renders a runnable file for `billet init` and is importable so tests can exercise what it writes.

Each invariant below is one line here and stated in full, with the incident or measurement behind it, in the reference file its group names. Read the reference before changing anything the invariant covers.

## Invariants

### Every block of billet.yaml, and who reads it: [references/blocks.md](references/blocks.md)

### Values and validation: [references/values-and-validation.md](references/values-and-validation.md)

- **The node's upgrade acknowledgement needs a short absolute state-directory path.**
- **`config` is a leaf, and `alloc.New` re-applies the safety rules anyway.**
- **`release.automatic` is the one zero value that does not refuse, and it is a pointer so absence and `false` differ.**
- **Never guess a byte size.**
- **Normalise a value something else will use; refuse an identity.**
- **A rule about a string that leaves the process is pinned to the boundary it crosses.**
- **Small refusals that each cost a debugging session.**
- **Cache TLS interception defaults off, per tier.**
- **`tiers[].cache` is resolved once, by `Tier.EffectiveCache`, and a node receives the result (#226).**

### Hosts, macOS and sites: [references/hosts-and-sites.md](references/hosts-and-sites.md)

- **A contribution is detected only for a backend that runs work on the host.**
- **The macOS limit is a default about a host somebody owns.**
- **A macOS tier pins a node unless it spans several backends.**
- **Storage is a property of the site.**

### The commands that write billet.yaml: [references/writers.md](references/writers.md)

- **Two commands write `/etc/billet/billet.yaml` under different rules, and each says which; a third writes a directory.**
- **`billet init` refuses what it cannot measure.**

Measured facts for this area, dated: [references/measured-facts.md](references/measured-facts.md).

## Checklists

**A new key or validation rule.** `internal/config` imports nothing of billet's, so a rule `alloc` also needs is exported from `config` and called from both. Refuse an identity; normalise only a value something else will use. Add the key to the blocks table ([references/blocks.md](references/blocks.md)), `billet.example.yaml` and `docs/reference/configuration.md`, and build test cases by `strings.Replace` on a known-good config, covering both directions of the guard.

## Where the tests are

- `internal/config/*_test.go`: build cases by `strings.Replace` on a known-good config; both directions of every guard (the macOS cap has one test proving a tier whose label omits `macos` is capped and one proving `builds-macos-artifacts` is not).
- `internal/integration/configboundary_test.go`, `generatedtier_test.go`, `releaseboundary_test.go`.
- `internal/ops/setup/configedit_test.go`, `init_test.go`, `init_rerun_test.go`, `initansible_test.go`, `inittart.go`'s tests, `inithybrid_test.go`; `internal/initconfig/*_test.go`, `internal/initconfig/hybrid_test.go`.

## Related skills

`billet-capacity` (what the numbers become at runtime), `billet-providers-local` and `billet-providers-aws` (per-backend blocks), `billet-storage-and-cache` (sites), `billet-identity-and-ca` (the App block and `github-app create`).
