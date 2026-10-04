# The layering and the bans

Part of the `billet-checks-and-lint` skill. Each rule is stated in full, with the measurement or incident behind it.

**The depguard rules are the architecture.** Read them in `.golangci.yml`; the ones that carry weight:

- `config` may import no other billet package. It is a leaf, so validation rules that `alloc` also needs are exported from `config` and called from both.
- `provider` and `store` are siblings below the scheduler: neither imports the other, neither imports `server`, `node` or `cmd`.
- `ledgerwriters` (`internal/state`, `internal/alloc`, `internal/rollout`, non-test files) may not import `net/http`, `os/exec`, `provider`, `store`, `github`, `scaleset`, `nodeplane`, `nodeclient`, `node`, `server` or `cmd`. `DB.Tx` begins IMMEDIATE and holds SQLite's single writer slot from BEGIN, so a network call inside a transaction stalls every scheduling write. Measured at zero violations before it went in, which is the bar for any rule in CI.
- `sqlitedriver` confines `modernc.org/sqlite` and `github.com/jackc/pgx` to `internal/state`, which verifies WAL and `synchronous=FULL`, takes the process lock and serialises writers.
- `scalesetclient` confines `github.com/actions/scaleset` (a public preview) to `internal/scaleset`.
- `generatedqueries` lets only `state`, `alloc` and `rollout` import `internal/state/ledgerdb`, for its parameter and row types; the handle comes from `state.ReadQueries`/`state.WriteQueries`, and `forbidigo` bans `ledgerdb.New` everywhere but the direct members of `internal/state`.
- `sqlcgenerated`: generated code imports nothing of billet's.
- `lifeops` is a strict allowlist (`$gostd`, `deploy`, `config`, `lifeops`, `golang.org/x/sys/unix`), every entry `$`-anchored because depguard matches a prefix; `unix.Access` is admitted so a `--dry-run` can ask "can this account write here" without writing.
- Global bans: `github.com/pkg/errors`, `io/ioutil`, logrus/zap/zerolog (billet uses `log/slog`), `math/rand` (use v2 or `crypto/rand`), `github.com/mattn/go-sqlite3` (cgo ends the single static binary).

**The forbidigo bans and why.** `fmt.Print*` (operator output belongs to `cmd/billet`), `panic` (a control plane that panics drops every in-flight lease), `os.Exit`, `http.Get/Post/PostForm/Head`, `context.Background/TODO`, `time.After` (leaks its timer until it fires), `errors.As` (use `errors.AsType[T]`, so a target cannot be used outside the branch that proved it), and `ledgerdb.New`. `analyze-types: true` so an import alias does not evade a ban. `inamedparam` requires named interface parameters because an interface is a contract somebody else implements.

**Exclusions are anchored with `(^|/)`, and that was measured.** golangci-lint's cache is keyed on file content and shared across checkouts; a `^cmd/billet/` anchor stopped matching in a second checkout and 612 previously excluded findings came back as failures against a tree with nothing wrong. Test files relax `gosec`, `contextcheck`, `containedctx`, `nilnil`, `nonamedreturns`, `forbidigo`, `revive`, `dogsled` and `prealloc`, and deliberately not `errcheck`: measured at 19 sites, two of which were real bugs. `.claude/` is excluded because sessions park worktrees there and a pass was linting another branch's half-finished code.

**One gRPC server, in one package.** The depguard rule `remoteapis` confines `github.com/bazelbuild/remote-apis`, `google.golang.org/grpc`, `google.golang.org/genproto` and `google.golang.org/protobuf` to `internal/node/reapi`, which serves the cache half of the Remote Execution API over a `Volume` the node hands it and knows nothing of sessions or publication. A test that needs a real gRPC client lives in `reapi_test`; a node test replaces `CacheService.remoteAPI` with a recording handler instead of importing gRPC. Adding the API cost 462,848 bytes (1.2%) of a stripped linux/amd64 binary, because gRPC was already linked through an existing dependency (2026-09-25).
