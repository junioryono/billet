# Reading the ledger back: job statistics, tier recommendations and workflow rewrites

What `billet jobs stats` (#535), `billet jobs rightsize` (#539) and `billet migrate workflows` (#538) were checked against before they shipped, and what that checking changed. Unless a line says otherwise, every reference-deployment number here was read on 2026-11-04 from a read-only copy of the reference control plane's SQLite ledger. The copy was taken with SQLite's online backup from a connection opened `mode=ro`, so the live ledger was never migrated or written, and the branch's binary was run against the copy on the shared Linux test host. That deployment's node is `ubuntu-01`. It serves billet's own CI (`junioryono/billet`, which is public) and an organization's repositories, which this record does not name. GitHub's side was read with `gh api` from its REST record of the same runs.

## A week of jobs, checked against GitHub

The copy held 7,277 jobs assigned in the seven days before it, 7,214 of which GitHub started: 6,900 succeeded, 295 failed, 1 was canceled and 18 had no result recorded. Every row had its repository, workflow, job name and run. 50 had no GitHub queue time, and 19 had a usage report that was missing (7,195 of 7,214 had one). `billet jobs stats --since 7d` read the whole deployment's week in 3.4 seconds.

Billet's own CI, `--repo junioryono/billet`, for 2026-10-28 07:40 to 2026-11-04 07:40 UTC: 78 jobs in 8 runs, all succeeded, duration p50 655 s, p90 825 s and p99 9,301 s (a guest image build), queue wait p99 1,390 s, 70.8 vCPU-hours, 406 kJ of active package energy over 77 jobs with one not measured, and no bought shape. Each of the 78 rows was found in its run's REST job list by its runner's name, `billet-<lease>`, which is how the control plane finds it:

| Check | Agreed |
|---|---|
| GitHub's conclusion against the ledger's result (`success` as `succeeded`) | 78 of 78 |
| GitHub's job name against the ledger's | 78 of 78 |
| Attempt, from GitHub's record | all 78 were attempt 1 |

The ledger's duration runs from billet's record of the start to its record of GitHub's result, both on billet's clock, and GitHub's from its `started_at` to its `completed_at`. Billet's ran shorter by 18.7 s at the median of the 78, and the difference ranged from 120 s shorter to 278 s longer (p90 55 s longer). It is the delivery of the two messages: billet records a start when it handles JobStarted and a result when it handles JobCompleted, either of which can arrive late. The report says which clock its duration is on.

On the tiers, the 16 vCPU pool ran 258 jobs against 523 the week before, at 88.3% succeeded (+11.6 points) with p50 9 m 17 s against 22 m 13 s, and the 8 vCPU pool 612 against 1,044. The week before also held the canceled runs of the 2026-10-25 CI queue, which the 16.3% and 13.3% canceled rates there show.

## Flakes need the run attempt, and GitHub's record of a re-run

No scale-set message carries a run's attempt, so migration 71 adds `github_run_attempt` and the queue-time sweep records it from the same REST read of a run that gives it the queue time. The reference ledger predates migration 71, so its week shows `flaky not recorded (attempt recorded for 0 of 258 jobs)` and so on per group, never a rate of zero.

To check the judgment on real re-runs, the copy was written with the attempts GitHub's REST record gave for six of billet's own runs from 2026-10-21 to 25 that had been re-run (nineteen more could not be read, answering HTTP 502 that day). `billet jobs stats --since 14d --by job --repo junioryono/billet` then judged 10 job runs, all failed and then passed, which is what the six were chosen for: `ci.yml` `test` 4 of 4, `ci.yml` `Verify All Checks Passed` 4 of 4, and `guest-image.yml` `Build and check` 2 of 2.

The same read found what a re-run of failed jobs does to the passed ones. GitHub lists each one again under the new attempt with the same runner, a new id, the same `started_at` and `completed_at`, and a `created_at` of when the re-run was asked for. Run 39813415826's job on one runner is listed as attempt 2, created 17:09:06, and as attempt 3, created 20:03:23, both started 17:22:38; run 40218475152 lists four such pairs. A runner therefore names one job only together with the lowest attempt it is listed under, which is the one that ran it, and that entry's `created_at` is the job's queue time. The sweep's read keeps that entry and drops the copies. It drops a name the attempts cannot separate entirely.

The sweep's read itself was run against GitHub on 2026-11-05 through billet's own client with the test organization's App. `billet-testbed/jobs`' `queue` workflow was dispatched on `ubuntu-latest` as run 42126907753, then re-run whole. `RunJobRecords` read runner `GitHub Actions 1000000006` as attempt 1, queued 23:05:44, and `GitHub Actions 1000000007` as attempt 2, queued 23:06:04, which is what `gh api` lists for the run.

## What a recommendation can read

The first `billet jobs rightsize` read the usage summary, and on the reference week it recommended `larger` for 39 of the 43 jobs with ten runs, 30 of them for memory. Both of its figures were wrong for this, and a probe over the 4,262 runs of the 72 hours whose per-sample detail the copy still held measured why:

- **The summary's window is the lease's life, not the job's.** A pooled runner can stand idle for hours before GitHub gives it a job, and the usage window ran from 1.09 times the job's duration at p10 to 1.61 at the median and 41.5 at p90, up to about 1,600. CPU time over the window understates a job's busy fraction, and CPU time over the job's duration overstates it, past 1,000 for one job. The host's per-sample series, integrated over the job's own window from its start to its result, covered at least 97% of the run for every job's median, so a run is measured from its series.
- **The host's memory peak counts what is not the job's.** It is the cgroup's `memory.peak`, which holds the page cache and the VMM. At p95 it read from 50% to 284% of the job's charged memory across the 43 jobs, 24 of them over 100%, and no job of the 7,360 in the copy's week was OOM killed. The job's own VM's highest MemTotal less MemAvailable over its MemTotal ran from 12% to 61% for the same jobs, with its MemTotal 96% to 98% of the charged memory. Host memory pressure stalled under 0.2% of every job's life.

So a recommendation reads CPU from the host's series over the job's window, and memory from the job's own VM, from a report the node kept whole. The report is labelled as the VM's own unverified figure, and the job runs as root there and could change it; it only advises, and the operator applies it. The host's peak is printed beside it and decides only that a job stayed small. Tart and macOS guests send no VM report, so they get `smaller` only when even the host's peak is small.

Over 2026-10-28 07:40 to 2026-11-04 07:40, the copy's 67 jobs came to 30 `keep`, 7 `larger`, 6 `smaller` and 24 with no recommendation, every one of those under the 10 measured runs it needs. 19 were high confidence, 17 medium and 7 low. The seven `larger` are 1 vCPU jobs CPU-bound at 89% to 101% busy at p90, each to the 2 vCPU tier. Of the six `smaller`, one 2 vCPU job at 12% busy and 17% memory goes to the 1 vCPU tier, and five 1 vCPU jobs at 23% to 29% busy have no smaller tier, which the report says. Reading each measured run's series and VM report took 94 seconds.

Two jobs were recomputed by hand through another path: `billet jobs show --json --window <start>,<result>` for each of their runs, the busy fraction from its window's CPU and the VM's memory from the report it prints, with the percentiles taken by hand.

| Job | Recommendation | By hand | The report |
|---|---|---|---|
| a 2 vCPU job | `smaller`, to the 1 vCPU tier | 20 runs, busy p90 12.44%, VM memory p95 16.7% over 20 | 20 runs, busy p90 12.21%, VM memory p95 16.7% over 20 |
| a 1 vCPU job | `larger`, to the 2 vCPU tier | 36 runs, busy p90 101.32%, VM memory p95 27.1% over 36 | 36 runs, busy p90 101.32%, VM memory p95 27.1% over 36 |

The first job's busy fraction differs by 0.2 points because `jobs show --window` aligns a window to the second. A busy fraction over 100% on 1 vCPU is the VMM's threads, which the cgroup counts.

## GitHub's hosted labels

`billet migrate workflows` maps a hosted label by the shape GitHub's hosted-runner reference gave on 2026-11-03: 2 vCPU and 8 GB on standard Linux, `-arm` included, in a private repository, and 4 and 16 in a public one; 1 and 5 on `ubuntu-slim`; 3 and 7 on arm64 macOS (`macos-latest`, `macos-15`, `macos-26`, `xcode-27`); and 4 and 14 on `macos-15-intel` and `macos-26-intel`. `macos-13` and `macos-14` are no longer in it. The larger macOS shapes (`-large` 12 and 30 on x64, `-xlarge` 5 and 14 on arm64) need a paid plan and are documented, not measured.

On the reference deployment's tiers the mapping finds an architecture for every one: `ubuntu-2404-x64@verified` names x64 and 24.04, the Linux tart tier is arm64 because tart is, and the macOS tiers are arm64, with their versions read from `macos-tahoe` (26) and `macos-golden-gate` (27). A docker tier on `ghcr.io/actions/actions-runner:latest` names neither, so it is never chosen without `arch` or `--map`.

## What is not covered

- Flake rates on the reference deployment start with the release that carries migration 71. Its last week judged none, and only the re-runs written into the copy above were judged.
- A flake needs both attempts to have run on the same deployment, the job to have started within the day the sweep reads back, and a run of at most 200 jobs.
- A recommendation is made from the runs whose per-sample series is still kept, 72 hours by default, whatever `--since` says.
- Nothing here ran `billet migrate workflows --open-pr` against a real repository. Its tests run it against a local git repository with a bare origin and a stub `gh`.
