# Adversarial correctness review: `979dd95`

Scope: source review only. I did not build, test, vet, lint, or read either skill directory. I began with `git show --stat 979dd95` and inspected only files changed by the commit, plus their parent revisions where needed to reason about old/new wire behavior.

## Findings

### P1 — `Recover` still treats an accepted EC2 termination as sufficient cleanup and allows admission to continue

`internal/node/runner.go:629-648` calls `provider.Destroy`, discards its `Teardown` result, then calls `releaseOrphanedLease` and continues recovery as long as the API request itself did not error. For EC2, every successful terminate returns `TeardownRequested` (`internal/provider/ec2/ec2.go:1138-1146`), including while `shutting-down` is deliberately classified as potentially executing (`internal/provider/ec2/ec2.go:1309-1325`). This is not merely a diagnostic mismatch: `Recover` runs before listeners open (`internal/node/runner.go:534-552`), and its stated purpose is to keep startup from admitting work while an orphan still overcommits the host. The new comment explicitly reverses that safety property by saying an accepted teardown should not make startup refuse work (`internal/node/runner.go:632-638`). Consequently a server/node restart can admit a replacement job while the orphaned EC2 guest is still executing shutdown work. `releaseOrphanedLease` will also terminalize a lease if one is found at that point (`internal/node/runner.go:927-942`), despite the teardown being unconfirmed. This call site should treat `TeardownRequested` as unresolved, just as `Sweep` now does at `internal/node/runner.go:824-847`.

### P1 — A second `Runner.Destroy` converts an outstanding custody handoff into plain success before the guest is gone

On the first asynchronous teardown, `Runner.Destroy` removes the request from `running`, adds a custody entry, and returns `ErrCustody` (`internal/node/runner.go:427-479`). On every later destroy for the same request, it first invokes `releaseRequest` (`internal/node/runner.go:397-415`). `releaseRequest` calls `tendOne`, but `tendOne` returns nil when the backend again says only `TeardownRequested` (`internal/node/custody.go:440-468`), and `releaseRequest` therefore returns nil while retaining the custody entry (`internal/node/custody.go:599-612`). Because the request was removed from `running` by the first call, `Runner.Destroy` then takes its idempotent-success branch (`internal/node/runner.go:417-424`). Any caller that retained the lease and retried now reads this as confirmation and can release capacity while the instance is still present. The new tests cover first destroy, one tend, and eventual disappearance (`internal/node/runner_test.go:212-280`), but never issue a second `Destroy` while custody remains.

### P1 — Mixed broadcast results cannot safely discard custody in favor of an ordinary error

The new precedence at `internal/nodeplane/runner.go:208-236` removes every `ErrCustody` leg whenever any other leg has a real error. Its claim that “Nothing is lost by dropping it” (`internal/nodeplane/runner.go:215-219`) is false because custody grants the node authority to release the lease, not just authority to heartbeat it. In the restart/broadcast case, node A can return custody and independently release the lease as soon as its instance disappears, while node B's timeout or destroy failure still means another copy may exist. The listener retaining and heartbeating the lease cannot prevent node A's release. A retry makes the failure faster: because of the preceding finding, node A answers plain success on the second destroy even while its custody entry and instance remain, and the listener can then release immediately. This is the harmful form of double ownership. A correct aggregate needs to preserve both facts—at least one leg owns custody and at least one leg remains unresolved—and must not let any one leg terminalize the shared lease until all possible compute is confirmed gone.

### P1 — The unchanged wire version is unsafe in both rolling-upgrade directions

The commit makes `CommandResult.Custody` semantically required for destroy responses at `internal/nodeclient/loop.go:760-775` and adds the corresponding server interpretation at `internal/nodeplane/runner.go:388-405`, but does not bump the protocol version.

Old node → new server is immediately unsafe: the old node has no destroy-side custody response and reports success as soon as EC2 accepts `TerminateInstances`; the new server therefore treats the destroy as confirmed and releases the lease.

New node → old server is initially fail-closed but becomes unsafe on retry: the old server turns `OK=false, Custody=true` into an ordinary destroy error, so the listener retains the lease and schedules an immediate retry (`internal/server/listener.go:2472-2515`). The new node's second `Destroy` then follows the custody-to-success bug above and returns `OK=true` before the guest is gone. Not bumping is therefore not compatible. If mixed versions are supported at all, this change needs a version gate or protocol bump that prevents these pairings.

### P2 — Teardown custody can hold capacity forever; the reaper is not an automatic escape while the node remains alive

An async teardown creates a discarded custody entry. The configured maximum is applied only to adopted entries (`!c.discard`) at `internal/node/custody.go:361-372`, so it cannot bound this case. Every `Find` error returns without advancing the entry (`internal/node/custody.go:406-409`), while the independent keepalive continues renewing it (`internal/node/custody.go:215-275`, `internal/node/custody.go:305-321`). An instance stuck forever in `shutting-down` similarly receives another accepted terminate every tend and never reaches `finish` (`internal/node/custody.go:433-475`). Thus a persistent provider error or permanently stuck instance can reserve the node's capacity forever. This is fail-closed and safer than reuse, but it is an unbounded operational outage, not a bounded recovery path.

If the node process dies, heartbeats stop and the reaper can quarantine the lease, but quarantine deliberately keeps capacity charged. The changed code's recovery path is a node returning and reporting inventory: `Recover` adopts quarantined compute (`internal/node/runner.go:568-625`), and every `Sweep`, including an empty one, reports inventory so a sufficiently aged quarantine can resolve (`internal/node/runner.go:745-771`; the behavior is exercised at `internal/node/runner_test.go:1452-1516`). If the node never returns, or provider listing keeps failing, there is no automatic proof of absence in the reviewed code; the test commentary identifies `billet leases release --force` as the prior/manual escape (`internal/node/runner_test.go:1452-1463`). The reaper can act by quarantining, but it cannot safely free the capacity by itself.

## Answers to the seven questions

### 1. Every `provider.Destroy` call site — Grade P1

There is one remaining production path that treats an unconfirmed teardown as enough to continue freeing/admitting capacity: `Recover` at `internal/node/runner.go:629-648`, described in the first finding.

The other production call sites are as follows:

- `cmd/billet/images.go:459-482` ignores the state for a firecracker-only probe with no lease. P3, fine.
- `internal/node/custody.go:440-475` retains custody unless the result is `TeardownStopped`. P3, fine.
- `internal/node/runner.go:427-479` hands the first unconfirmed teardown to custody. The first call is fine, but its retry behavior through `internal/node/runner.go:397-424` is P1 as described above.
- `internal/node/runner.go:492-531` (`destroyStray`) returns `confirmed=false` unless stopped, causing the launch path to hold the lease at `internal/node/runner.go:357-372`. P3, fine.
- `internal/node/runner.go:629-648` (`Recover`) ignores `TeardownRequested`, may call `releaseOrphanedLease`, and allows startup to proceed. P1.
- `internal/node/runner.go:821-855` (`Sweep`) skips release unless stopped. P3, fine.
- `internal/provider/ec2/build.go:74-85` ignores the state for a lease-free AMI builder; it only needs to submit termination and has no capacity ledger to release. P3, fine for this question.
- The remaining calls in changed files are tests or test cleanup. They do not release production capacity.

The three listener calls (`internal/server/listener.go:1424`, `internal/server/listener.go:2292`, and `internal/server/listener.go:2442`) invoke the higher-level `server.Runner.Destroy`, not `provider.Destroy`. Shutdown and normal completion correctly stand down on `ErrCustody` (`internal/server/listener.go:1426-1462`, `internal/server/listener.go:2442-2469`). The reclaimed-during-launch path at `internal/server/listener.go:2288-2298` owns no live listener lease by then, so its failure to special-case custody does not itself release capacity.

### 2. Double-holding — Grade P1 overall

The brief ordinary handoff overlap is benign: the node records custody before returning `ErrCustody`, so the listener and node may heartbeat the same lease until the listener deletes its `running` entry at `internal/server/listener.go:2459-2467`; heartbeats at the same epoch are idempotent.

Long-lived double-holding occurs when custody is masked by another broadcast error (`internal/nodeplane/runner.go:208-236`) or by an old server that does not interpret destroy-side `Custody`. It is harmful, not merely redundant, because the custody node can independently release the one shared lease while another node remains unresolved, and because a retry converts the custody node's answer to success before disappearance (`internal/node/runner.go:397-424`; `internal/node/custody.go:440-468`).

### 3. Forever holds and the reaper — Grade P2

Yes. A discarded teardown entry is outside `maxCustody`, repeated `Find` errors do not age it out, and a forever-present/stuck instance never reaches `finish` (`internal/node/custody.go:361-475`). While the node lives, keepalive prevents the reaper from acting. After node death, the reaper can quarantine when renewal expires, but quarantine intentionally does not free capacity. The path back is a recovered node reporting the instance gone through periodic inventory (`internal/node/runner.go:745-771`), or an operator force-release; if the node never returns and there is no authoritative inventory, the safe state is an indefinite hold.

### 4. Broadcast precedence — Grade P1

The precedence is not correct. A real error must not be dropped, but a custody answer cannot be dropped either. Custody means that leg now has independent release authority; suppressing the signal does not revoke that authority. The aggregate needs to represent both an unresolved failure and outstanding custody, and it must prevent lease terminalization until every possible leg is settled. The current claim at `internal/nodeplane/runner.go:215-219` loses that fact and is directly unsafe in combination with `internal/node/runner.go:397-424`.

### 5. Wire compatibility — Grade P1

Not bumping the version is incorrect. Old node → new server releases immediately on EC2 acceptance because the old node never sets destroy-side custody. New node → old server treats custody as a plain failure, retries, and can receive false success from the second-destroy path before the guest disappears. The newly required producer and consumer are visible at `internal/nodeclient/loop.go:760-775` and `internal/nodeplane/runner.go:388-405`; because the version file was not changed and the review was restricted to changed files, I did not open it.

### 6. `observed=false` and stray grace — Grade P3, fine as a fail-closed consistency defense

Setting `observed=false` at `internal/node/custody.go:134-152` correctly covers the `RunInstances`/`TerminateInstances` NotFound window because `TeardownRequested` deliberately conflates an accepted terminate with `InvalidInstanceID.NotFound` (`internal/provider/ec2/ec2.go:1107-1133`). A single missing `Find` therefore cannot be trusted, and `internal/node/custody.go:411-426` waits through `strayGrace` before releasing.

It can delay capacity return by up to five minutes when an accepted termination completes before the first tend, even for a long-running job whose instance had unquestionably existed. Usually the first tend still sees EC2's one-to-two-minute `shutting-down` state, sets `observed=true` at `internal/node/custody.go:429-431`, and the eventual disappearance releases immediately. The residual delay is conservative and safe; with the current two-state `Teardown` result, removing it would reopen #48. A third result distinguishing “termination accepted for this known id” from “NotFound, existence unknown” could avoid the unnecessary delay.

### 7. Commit-message accuracy — Grade P1

The commit materially improves the primary completion, custody-tend, stray-cleanup, and sweep paths, but it does not fully deliver its global claims.

- “every caller read ‘no error’ as ‘the compute is gone’” is overstated: the AMI builder and firecracker probe only submit cleanup and hold no lease, while `Recover` is the important caller that still discards the new state.
- “An unconfirmed teardown takes that path” is false for `Recover` (`internal/node/runner.go:629-648`), which accepts `TeardownRequested` and proceeds, and it is not stable across a second `Destroy` (`internal/node/runner.go:397-424`).
- “Custody already means ‘I am holding this lease’s capacity and will release it when the compute is provably gone’” is true for one isolated node, but incomplete for a broadcast: one custody leg may release the shared lease while another leg is unresolved (`internal/nodeplane/runner.go:208-236`).
- “The same trap ... is closed too ... Sweep released an orphan’s lease the same way” is true for `Sweep` (`internal/node/runner.go:821-847`) but not for the parallel startup reconciliation in `Recover`.
- “Nothing is lost by dropping [custody]” in the implementation comment (`internal/nodeplane/runner.go:215-219`) is the most consequential overstatement: the dropped signal carries release ownership.

No P0 issue was found. The P1 issues can reproduce the exact invariant violation the change targets: capacity becomes reusable while EC2 compute may still execute.
