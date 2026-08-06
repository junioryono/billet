# billet

Self-hosted GitHub Actions runners on your own hardware, with a colocated cache.
Go, Apache-2.0, single static binary. Pre-alpha.

## How to Work on This Codebase

### Skills First

Skills encode multi-step workflows and hard-won conventions. Before exploring code or planning an
approach, check which skills apply and load them. Skipping one means rediscovering something that is
already written down.

1. **Load skills before researching.** They contain the context you need.
2. **Create skills for new patterns.** Repeatable multi-step work with no skill → suggest one via
   `skill-creator`.
3. **Keep them current.** If a change invalidates something a skill says, update the skill in the
   same PR.
4. **Keep CLAUDE.md current.** Same rule.

### Codex compatibility (AGENTS.md + .agents/skills are symlinks)

This repo supports OpenAI Codex, which reads the cross-tool standard files. **Claude's files are
canonical**; the standard paths are committed symlinks. Never create a real file at a symlink path,
and never edit `AGENTS.md` (you would be editing the `CLAUDE.md` it points to).

- `AGENTS.md -> CLAUDE.md`
- `.agents/skills/<name> -> ../../.claude/skills/<name>` for every skill. **Add the symlink in the
  same PR that adds a skill**, and keep SKILL.md frontmatter strict YAML — quote any description
  containing `:`, since Codex rejects invalid YAML that Claude tolerates.
- `.codex/config.toml` raises `project_doc_max_bytes`; keep it if you touch Codex config.

### Verify your work

`make check` is the gate. It runs build, vet, gofmt, lint, and `go test -race`. All of it must be
clean before a commit. There is no "the linter is being annoying" — see the lint section below.

---

## Architecture

One binary, two roles. `billet server` is the control plane: it long-polls GitHub's Runner Scale Set
API for assigned jobs, owns the capacity ledger, and tells nodes what to launch. `billet node` is a
compute host: it runs a provider and launches instances. `billet server --dev` runs both in one
process — the single-machine deployment.

```
cmd/billet/          the binary: server | node | dev roles, plus the whole operator CLI
internal/config/     billet.yaml schema + validation (a leaf package — imports nothing of ours)
internal/state/      SQLite control-plane store: capacity ledger, job history, process lock
internal/server/     scale-set listeners, global capacity allocator, scheduler   (P1)
internal/node/       node runtime: provider driver, capacity reporting, mTLS     (P2)
internal/provider/   firecracker | tart | ec2 | docker                            (P1+)
internal/store/      zfs | ebs | apfs — CoW clone, generations, atomic publish   (P3)
internal/cachev2/    GitHub Actions Cache v2 Twirp + conformance suite           (P4)
```

Layering is enforced by `depguard` in `.golangci.yml`, not by convention: `provider` and `store` are
siblings that may not import each other or the scheduler, and `config` may not import any other
billet package.

---

## Invariants

These are the rules that a change can silently break. Each one exists because the alternative was a
real failure mode, not a preference.

### The control plane has exactly ONE authoritative writer

Corrupting the capacity ledger means double-admitting jobs onto a machine that cannot hold them, and
the failure is quiet — jobs are accepted, then the host OOMs or thrashes.

- **An exclusive `flock` on the state directory is held for the DB's lifetime.** SQLite's own
  single-writer rule prevents simultaneous *writes*; it does not prevent two billet processes both
  long-polling GitHub and taking turns writing conflicting decisions. The second process must not
  start at all. `flock` rather than a PID file, so a crash releases it automatically.
- **Writes go through `DB.Tx` on a one-connection pool.** An allocation decision is read-current,
  decide, record — one transaction, not a read followed by a hopeful write.
- **`DB.Reader()` returns a narrow `Querier`, never `*sql.DB`.** Handing out the pool would let any
  caller write through a connection that is supposed to be read-only.
- **The database MUST be on local storage.** SQLite's WAL is explicitly unsafe on a network
  filesystem. `Open` reads the pragmas back and fails closed if `journal_mode` is not WAL, which is
  what catches an NFS state directory.

### Migrations are append-only and identified by version + checksum

Never edit or reorder an existing migration; add a new one. `migrate` verifies a recorded checksum
against the binary's SQL and refuses on mismatch, and refuses a database carrying a version this
binary has never heard of. Counting applied rows is not a version — a deleted row reruns a
migration, a forged row skips one, and inserting one in the middle silently reruns the tail.

### Cache TLS interception defaults OFF, per tier

`ACTIONS_RESULTS_URL` carries **artifact** metadata as well as cache traffic, so anything in that
path is in the user's release path. `Tier.Intercept` is opt-in, and tiers that publish release
artifacts or hold deployment secrets must not enable it. A mistake here does not slow CI down; it
breaks a deploy.

The protocol is reverse-engineered — GitHub has never published the `.proto` files — so the cache
must **fail open to a miss** on any error, never fail a job, and a conformance suite runs the real
`actions/cache`, `upload-artifact` and `download-artifact` against live GitHub to catch drift.

### Apple's 2-VM limit is enforced against `guest_os`, never a label

Apple's licence permits at most two macOS guests per Apple-branded host. Keying that off a label
matching `macos` means a tier named `sonoma-arm64` escapes it entirely, and a Linux tier named
`builds-macos-artifacts` gets capped for no reason. `Tier.GuestOS` is the explicit field, macOS
tiers must pin a `node`, and per-node totals are summed at load. Warm instances count.

The config check is a **guard, not the enforcement point**: the allocator must hold a single
host-wide count of running plus warm macOS guests at runtime, because two individually-valid tiers
still share one physical Mac.

### Capacity is escrowed BEFORE a listener advertises

Each tier is its own GitHub scale set with its own `maxCapacity`. If listeners advertise
independently, GitHub can fill all of them at once and the host is overcommitted with nothing to
stop it. Reserve against the global ledger first, advertise second. Capacity is a **vector** — CPU,
memory, macOS licence slots, disk — never one integer.

### Never guess at a byte size

`config.ByteSize` parses with exact integer arithmetic on a restricted grammar. It used
`strconv.ParseFloat`, which accepts `NaN`, `Inf`, hex and exponents, and loses precision above 2^53 —
and converting any of those to `int64` is implementation-defined and can come out **negative**, which
silently disables the capacity ceiling. Reject what cannot be represented exactly.

---

## Linting

`.golangci.yml` is the source of truth for Go conventions, and the *why* for each non-stdlib rule is
a comment next to that rule. The version is **pinned to v2.12.2** and CI uses the same one:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
```

`nolintlint` rejects a bare `//nolint`. An exception is written `//nolint:linter-name // reason`,
and the reason is mandatory — so "0 issues" means "0 unexplained issues".

**When you fix a bug, ask whether it should be a lint rule.** If the bug is a *class* something could
reintroduce, and it is mechanically checkable, encode it:

1. **Can golangci-lint express it?** A `forbidigo` identifier ban, a `depguard` layering rule, a
   linter setting. Cheapest — do this first.
2. **Project-specific but deterministic?** A `go/analysis` analyzer, wired into CI only if it
   measures ~zero current violations. A noisy analyzer erodes trust and gets ignored.
3. **Needs judgment?** Document it in the Invariants section above.

Rules already carrying real weight here:

- `depguard` bans `mattn/go-sqlite3` (cgo breaks the single-static-binary goal and the cross-build
  matrix) and confines `modernc.org/sqlite` to `internal/state`, so nothing else can open a
  connection without the durability pragmas.
- `forbidigo` bans `time.After` (leaks its timer until it fires), `panic` (a control plane that
  panics drops every in-flight lease), and `context.Background` outside `cmd/billet`.

---

## Testing

`make test` runs `-race`; `make cover` writes and opens an HTML profile.

- **A test must fail when the code is wrong.** Two of the original state tests did not: one read
  pragmas through the *reader* pool, where `journal_mode` reports `wal` from persistent file state
  regardless of the writer's DSN; the other exercised only the single-connection writer, which makes
  serialization tautological. If a test asserts an invariant, break the invariant once and confirm
  the test fails.
- **Assert the diagnostic, not the shape.** Counting error lines passes for the wrong reasons;
  asserting the specific messages does not.
- Tests use `t.Context()` and `t.TempDir()` (enforced by `usetesting`).
- `paralleltest` is deliberately off: these tests open real SQLite files, and `t.Parallel()`
  everywhere buys nothing and invites flakes.

## Coverage

Codecov is wired in CI. Current: **`internal/config` ~80%, `internal/state` ~79%**.

The project target is **`auto`** — do not regress from the base commit — not an absolute number, and
that is deliberate. A hard 85% on a project sitting at 79% fails every PR from day one, and a check
that always fails is a check everyone learns to ignore. New code carries an 80% patch target, so the
overall figure ratchets up as the project grows rather than being declared true in advance.

Coverage is a signal, not a goal: a test that exercises a line without asserting anything raises the
number and catches nothing. See the `billet-testing` skill for the mutation-test discipline that
separates the two.

## Commands

```bash
make check     # build + vet + fmt-check + lint + race test — the pre-commit gate
make build     # ./bin/billet
make test      # go test -race -count=1 ./...
make cover     # coverage profile + HTML report
make lint      # golangci-lint run
make lint-fix  # auto-fix what can be auto-fixed
make fmt       # gofmt -s -w .
make tidy      # go mod tidy
make cross     # build every target a node can run on
```

## Git workflow

Feature branches, never commit to `main`. See the `billet-git-flow` skill for branch naming, commit
format, and PR conventions.

## Platform support

Linux (amd64, arm64) and macOS (arm64). The `flock`-based state lock is `//go:build unix`; a Windows
port needs an equivalent, and `make cross` is what catches a build-tag mistake before it reaches a
node.
