# What GitHub.com records about a job whose runner ended

On 2026-11-04 billet measured, against GitHub.com, what [ADR-029](../decisions/adr-029-rerunning-disrupted-jobs.md) rests on and GitHub does not document: what its record of a job says when the runner ended under it, whether that can be told from a job failing on its own, and what the two re-run routes answer.

## How it was measured

`TestLiveDisruptedJob` (`internal/integration/disruptedjob_live_test.go`) held one message session on a throwaway scale set through billet's own client, started a real runner for each assignment, and recorded every message. What happened to each runner was done by hand from another shell, and GitHub's REST record was read with `gh api` and, for the App's own answer and billet's parsing of it, with `TestLiveRerunRecordAndRefusal`.

The jobs were the `queue` workflow of a private repository on a free organization that exists only for testing billet: one job with one `sleep` step. The runner was `ghcr.io/actions/actions-runner:latest` in Docker on billet's shared Linux test host, except the EC2 row, which booted billet's own runner image (`c7i.large`, x64) with the boot script `internal/provider/ec2` writes, and ended it with the same `TerminateInstances` call the backend's `Destroy` makes.

## What each ending leaves

| How the job ended | Scale-set `result` | REST job `conclusion` | The running step | "Complete job" step | When GitHub concluded it |
|---|---|---|---|---|---|
| `docker rm -f` (SIGKILL), 36 s into the job | `failed` | `failure` | `in_progress`, no conclusion | absent | 10 m 0 s after the job started |
| `docker rm -f` (SIGKILL), 19 s into the job | `failed` | `failure` | `in_progress`, no conclusion | absent | 10 m 1 s after the job started |
| `TerminateInstances` on EC2, 37 s into the job | `failed` | `failure` | `in_progress`, no conclusion | absent | 10 m 0 s after the job started |
| The job's own failure (`sleep` given a bad argument) | `failed` | `failure` | `completed`, `failure` | `success` | within seconds |
| `docker stop -t 120` (SIGTERM, a graceful stop) | `failed` | `failure` | `completed`, `failure` | `success` | within seconds |
| `gh run cancel`, a person's cancellation | `canceled` | `cancelled` | `completed`, `cancelled` | `success` | 56 s later |

Three things follow.

**A runner that vanished leaves a positive signature.** A step that never finished, on a job concluded `failure`, with no step concluded `failure`. Nothing else measured leaves it: a failing step always ends with a conclusion, because the runner that ran it reported it.

**A runner stopped gracefully cannot be told from a job's own failure.** On SIGTERM the runner cancels its job and reports the running step `failure`, and its own log says `completed with result: Canceled`, but the scale-set message, the REST job and every step are the same as a job that failed by itself. Nothing billet can read separates them, so [ADR-029](../decisions/adr-029-rerunning-disrupted-jobs.md) treats it as what it cannot be proved not to be.

**`TerminateInstances` is a vanished runner, not a graceful stop.** The runner in billet's EC2 image reported nothing in the time the machine took to go away. That is one job on one instance type, ended by the call billet makes when it destroys an instance, including on a Spot warning. A reclaim AWS performs itself, with billet not having terminated first, was not measured.

Why GitHub took ten minutes from the job's start, rather than from the runner's death, was not measured: all three were killed within the first minute of the job, and the time may be counted from the runner's last renewal instead. A job that finishes this way stays `in_progress` at GitHub for that long, so a re-run cannot be asked for sooner than that.

## What the re-run routes answer

| Request | On a run still going | With only `actions: read` | Otherwise |
|---|---|---|---|
| `POST .../actions/runs/{run}/rerun-failed-jobs` | 403 `This workflow is already running` | 403 `Resource not accessible by integration` | 201, body `{}` |
| `POST .../actions/jobs/{job}/rerun` | 403 `The workflow run containing this job is already running` | 403 `Resource not accessible by integration` | 201, body `{}` |

The two refusals on a running run were read with an owner's token, and the App's refusal with the test App's own, which holds `actions: read`.

Straight after a 201 the run reads `status: queued`, `conclusion: null`, **still on the attempt it was on**, and `jobs?filter=latest` answers `total_count: 0`. So whether a request landed is read from the run's status before its attempt says so, and an empty job list straight after a request means nothing about which jobs failed. Within a minute the run was on attempt 2 with a new job id, on a new runner, and `filter=latest` listed only that attempt while `filter=all` listed both. The run keeps its id across attempts. A job's own failure re-run this way failed again and concluded the attempt in seconds.

## What this did not measure

The test workflow has one job. So these rest on GitHub's documentation, not on a measurement, because creating a repository with a wider workflow was out of bounds for the run:

- Which conclusions `rerun-failed-jobs` counts as failed. billet therefore treats everything but `success`, `skipped` and `neutral` as failed when it decides whether a request is exact, and a job wrongly counted failed leaves the run for a person.
- That a re-run of one job also re-runs the jobs that `need` it.
- What a run with several jobs lists under `filter=latest` for the jobs an attempt did not re-run.

Also unmeasured: a job re-run through the App itself with `actions: write`, because the shared test App holds `actions: read`, and adding write would fail every other session's `billet check`. The routes' 201 was read with an owner's token.
