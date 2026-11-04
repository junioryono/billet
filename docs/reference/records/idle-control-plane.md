# What an idle control plane costs

On 2026-11-04 billet measured what fifty idle control planes cost on one host, and bounded what could be bounded. **An idle control plane is cheap: about 15 to 17 MiB of memory and under a tenth of a percent of a core. The number that adds up is its connections to a shared PostgreSQL**, four each, and those are now the operator's to set. Nothing else about idle needed fixing, and one bound this work first added made things worse, so it was taken back.

## How it was measured

`scripts/measure-idle-control-planes.sh` on one dedicated `m7i.2xlarge` (8 vCPU, 32 GiB), started fifty real `billet server` processes of six tiers each, no nodes, with GitHub's names in `/etc/hosts` pointing at a fake in `internal/integration/idlecost_test.go`. The fake answers a real client's whole handshake and parks each long poll for 50 s, as the real service does at no work. After a 10 minute settle it sampled every 60 s for 30 minutes: proportional set size from `smaps_rollup`, CPU from `/proc/<pid>/stat`, goroutines and heap from each plane's metrics, connections per peer from `ss`, and `pg_stat_activity` by `application_name`. A plane that stopped polling fails the run, so no number here measured a stuck deployment. PostgreSQL 18 ran in Docker on the same host. Each row is one run, medians across planes of each plane's median across samples.

`baseline` is the commit before this work, and `final` is the branch that shipped. The one real deployment below ran on the shared test host against a test organization's App.

## What it cost

| Run | Memory per plane (median, p95, max) | All 50 | CPU per plane, all 50 | Goroutines | PostgreSQL connections per plane at rest, fleet peak |
|---|---|---|---|---|---|
| SQLite, `baseline` | 17.2, 17.6, 18.1 MiB | 862 MiB | 0.053%, 0.027 cores | 47 | none |
| SQLite, bounds | 17.1, 17.5, 17.7 MiB | 854 MiB | 0.054%, 0.027 cores | 48 | none |
| PostgreSQL, `baseline` | 14.9, 15.4, 15.5 MiB | 745 MiB | 0.077%, 0.038 cores | 48 | 4, 200 |
| PostgreSQL, four idle readers (withdrawn) | 14.8, 15.1, 15.4 MiB | 739 MiB | 0.077%, 0.039 cores | 49 | 4, 226 |
| PostgreSQL, `final` | 14.7, 15.0, 15.6 MiB | 739 MiB | 0.078%, 0.039 cores | 49 | 4, 200 |
| PostgreSQL, `max_connections: 3` | 14.6, 14.9, 15.0 MiB | 729 MiB | 0.077%, 0.038 cores | 49 | 3, 150 |

The PostgreSQL server itself, shared by the fifty, held 1132 MiB at `baseline`, 1098 MiB at `final` and 1010 MiB at a budget of three: about 2.4 MiB for each connection saved. SQLite costs more per plane than PostgreSQL because each plane holds its page cache itself.

**A settled plane holds four connections: the writer, the controller claim's session, and two readers.** Two readers stayed in every sample, so a plane's own cadence keeps them in use and the one-minute idle timeout closes nothing on these planes; it matters only to one with no reads for a minute. Keeping all four readers idle, which this work tried first, raised the fleet's peak from 200 to 226 and lowered nothing, so a burst keeps `database/sql`'s own two. The extra goroutine is the pool's idle-connection cleaner.

**A target's polls share one connection.** All six tiers of a target held one connection to the fake's message host, on every plane, and none to the REST or pipelines hosts while idle. `TestATargetsLongPollsShareOneConnection` holds the client to that; real GitHub's connection count was not sampled.

**The memory is what it is, and a limit does not help.** Against the PostgreSQL `baseline` at no limit:

| `GOMEMLIMIT` | Memory per plane | CPU per plane |
|---|---|---|
| none | 14.9 MiB | 0.077% |
| 24 MiB | 14.8 MiB | 0.077% |
| 16 MiB | 12.0 MiB | 1.052% |

At 24 MiB the limit is never reached. At 16 MiB it saves 3 MiB a plane for fourteen times the CPU, because the collector is running constantly against a heap that cannot shrink further: 0.48 cores across the fifty, where nothing else here reached 0.04.

## What a restart costs

Against the fake, which has no session expiry, one plane reopened its six sessions in 46 ms on SQLite and 68 ms on PostgreSQL, and all fifty restarted together in 0.66 s and 1.33 s. That is billet's own part of a restart, and it is small.

**Against real GitHub a restart costs what GitHub charges for the session, clean or not.** One deployment of six tiers took 68.16 s and 67.55 s to have every session open again after a SIGTERM, and 67.90 s and 67.64 s after a SIGKILL. A clean stop is a handover: it closes no message session, so that what GitHub assigns meanwhile is redelivered to the next one, and so the next one waits out the old session's expiry either way. That is the 60 to 92 s bracket in `billet-github-protocol`'s measured facts. Queued jobs wait; none is lost.

## What was not measured

- **A connected node.** Every plane here had no nodes, so the node wire's cost per node is not in these numbers.
- **A standby.** A waiting standby's cost was read from the code, which is how its reconnecting every two seconds was found and fixed, rather than measured.
- **Work.** Everything here is idle by construction, so this says nothing of a plane running jobs.
- **Many deployments against real GitHub.** One deployment was restarted, to learn what a restart waits for. Fifty deployments' 300 sessions would be far past the twenty the test App has been measured to hold.
- **The fake's long poll is modelled, not measured:** 50 s and a 202. A real session's poll period sets the CPU row, not its memory.
