# Usage export

`billet usage export` prints what each finished lease was charged, in a format a billing system can consume without reading the ledger's tables, which are an internal format a migration may change. It only reports: nothing reads the export back, and no scheduling decision depends on it.

```bash
billet usage export [--config PATH] [--since CURSOR] [--limit N] [--json]
```

| Flag | Meaning |
|---|---|
| `--since` | resume after this record's cursor; empty starts at the first finished lease |
| `--limit` | the most records one run prints, 1 to 10000 (default 1000) |
| `--json` | one JSON object a line, rather than a table |

It runs on the control plane, beside a running server, and opens the ledger the way every operator command does.

## Reading it exactly once

Run it, keep the last record's `cursor`, and run it again with `--since` that cursor. A run that prints nothing has caught up. **A lease appears exactly once across any sequence of cursors**, through restarts, failovers and clock steps, with no gaps:

- Each lease takes the next number when it closes, in the transaction that closes it. The number is the lease's place in the order leases were numbered in, and it never changes.
- The number comes from a counter row that the closing transaction holds locked until it commits, so a number becomes visible only after every lower one. A reader that has seen N will never later find a lower number.
- Filtering is on the number alone. `finished_at` is a wall clock, which steps, and which a failover replaces with another machine's.

**A lease that finished before this release takes its number later,** in small batches the control plane works through in the background, so the upgrade rewrites no history at startup; it builds two indexes over it, which took 6.8 s on SQLite and 3.9 s on PostgreSQL for a million rows (2026-11-04, a loaded test host). Numbers are a place in the order of being numbered, not of finishing: `started_at` and `finished_at` say when a lease ran, and a billing period is the consumer's to cut from them. The command says on its standard error when older leases are still waiting, and they follow in later runs past the cursor it holds, with nothing skipped.

A cursor is opaque: it names the deployment, the number, the lease and a random mark drawn when that number was written. Store it and pass it back whole. An exporter that stores it after writing the records it covers reads a record twice at worst, after a crash between the two, and `lease_id` tells it so; one that stores it before can lose one.

**A resume refuses what it cannot honour, and prints nothing.** It refuses a cursor from another deployment, and a cursor whose number is missing, or holds another lease or another mark, on this ledger. The second means the ledger was restored from a backup older than the cursor: its next leases take numbers the exporter already read past, possibly giving the same lease the same number again, and reading past them would skip leases in silence. Reconcile what was exported after the backup, then start from the last cursor the restored ledger still holds.

Records carry what was known **when the lease closed and when the record is read**, and reading a lease once is not the same as having its final numbers. A late GitHub result fills a row that was missing one, so **a record with a `null` `billed_minutes` can gain one later, under the same number**: nothing moves it past your cursor, so keep such a lease and read it again from a cursor older than it. A redelivered completion can likewise turn `job_clock: billet` into `github`, with the interval it reads. An operator correcting a provisional verdict can change `conclusion`. A number and a lease do not change, and a `billed_minutes` read on `job_clock: github` does not.

## The record

Times are RFC 3339, on billet's clock except where a field says otherwise, and durations are seconds to the millisecond.

| Field | Meaning |
|---|---|
| `schema` | this format's number, 1. A change a consumer would have to read differently is a new number |
| `cursor` | what to pass to `--since` to read past this record |
| `sequence` | the lease's place in the order leases were numbered in |
| `lease_id` | the lease |
| `deployment` | the deployment's identity |
| `target` | the GitHub path of the owner the lease's runner was registered on: `owner` or `owner/name` |
| `repository` | the repository of the job the lease ran, as `owner/name` |
| `tier` | the tier's label |
| `site` | the site of the host it was placed on, empty where the deployment names none |
| `provider` | the backend it ran on |
| `ownership` | `owned` for a machine the deployment already had (`docker`, `firecracker`, `tart`), `bought` for one bought for this lease (`ec2`, `codebuild`) |
| `vcpu`, `memory_bytes` | the shape the lease was charged |
| `held` | `from` when the lease was bought, `to` when it closed, and `seconds` between: how long it held its capacity, launch and teardown included |
| `job` | `from` when GitHub gave the job a runner, `to` when GitHub says it finished, and `seconds` between |
| `job_clock` | whose times `job` holds. `github`: GitHub's own, from the messages, both ends on one clock. `billet`: GitHub's were not both carried, so these are when billet received the two messages, which can read shorter than the job ran: a real `JobStarted` arrived 46 and 47 s after its job started, and a job of 243 s read as 197 (2026-11-04). It can also read longer, by the length of an outage, for a completion written after a restart |
| `billed_minutes` | the job's time rounded up to a whole minute per lease, the way GitHub bills a hosted job |
| `minute_weight` | what one of those minutes counts as, in minutes of a 2 vCPU Linux runner |
| `weight_source` | `tier` when the tier sets `minute_weight`, `charged_vcpu` when it is the vCPU charged over two |
| `weighted_minutes` | `billed_minutes` times `minute_weight`, exact to three places |
| `instance_type` | the type bought, empty on an owned machine |
| `price_micros_per_hour` | what that type was charged at when the shape was charged, in millionths of a dollar; never today's catalogue. `null` and not in `unknown` on an owned machine, which bought nothing: `ownership: owned` says so |
| `cost_micros` | that price across `held`, rounded up to a whole millionth of a dollar. `null` and not in `unknown` on an owned machine, as above |
| `conclusion` | how billet's lifecycle ended the lease: `done` or `failed` |
| `result` | what GitHub said about the job, verbatim |
| `unknown` | each field above that the ledger does not know, and why |

## Minute weights

`tiers[].minute_weight` says what one minute of a tier counts as, so that minutes of different runners can be added up against one number. It is normalised to a 2 vCPU Linux runner: without it a 4 vCPU runner counts 2, from the vCPU the lease was charged rather than what the tier says today. Set it where vCPU alone does not say what a minute is worth, such as a macOS or an arm64 tier. It is a decimal above 0 and at most 1000, with at most three places.

```yaml
tiers:
  - label: billet-4vcpu-macos
    minute_weight: 10
```

The weight is read from the config when the record is read, so editing a weight changes what a re-read says of leases already closed: take the number when you first read it. A tier that declares none reads `weight_source: charged_vcpu`. A tier the config no longer declares falls back to the same arithmetic and reads `charged_vcpu_undeclared_tier`, because it may have named a weight this config cannot show. `minute_weight` is refused beside `sizes`, which would give every size one weight. Weights report and decide nothing: no code that admits, places or schedules reads one.

## Unknown stays unknown

A value the ledger does not know is `null` or empty, and `unknown` names it with the reason. The one `null` that is not unknown is a price on an owned machine, where there is none to know. It is never zero, because a billing system summing a column of zeros has quietly given minutes away. The cases, none of which is an error:

- **A lease that ran no job** and finished cleanly bills a known `billed_minutes: 0`, and `job` is empty with the reason. Its `held` is still there: it held capacity.
- **A lease that failed, was disrupted or passed through a node's custody with no start recorded** has an unknown `billed_minutes`. A `JobStarted` arrives tens of seconds late, so a lease that ended this way may have been running a job, and GitHub can still report one.
- **A job with no result yet**, which ended with the lease before GitHub reported one: `billed_minutes` is unknown and `held` says how long the lease lasted. What to charge is the consumer's decision, and billet does not guess one.
- **A result with no start**: GitHub reported a result and billet never saw the job start, so it has no length.
- **A clock that stepped** so that an end precedes its start: the length is unknown, not negative.
- **A row older than a column**: leases that finished before billet recorded a target (migration 72), a shape or a price (migration 49) say so. A bought machine with no price reads as unknown, never as free.
- **A backend this billet does not know**, written by a newer release: `provider` is kept as the ledger has it, and `ownership` is unknown.
