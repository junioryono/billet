# Alerts

The [metrics endpoint](metrics.md) lets you alert on the fleet. `server.alerts` lets a team hear about a job: one that failed or was cancelled, ran far longer than usual, waited too long for a runner, or was disrupted by billet's own infrastructure. It notifies a generic JSON webhook or a Slack incoming webhook, from the controller, out of the ledger, so a restart repeats nothing and a standby that takes over delivers what its predecessor owed.

```yaml
server:
  alerts:
    destinations:
      - name: oncall
        kind: slack
        url_env: BILLET_ALERT_ONCALL   # or url_file: /etc/billet/oncall-url, mode 0600
      - name: pager
        kind: webhook
        url_file: /etc/billet/pager-url
    rules:
      - name: deploys
        match:
          repository: "acme/*"
          workflow: deploy.yml
          branch: main
        on:
          failed: true
          disrupted: true
        in_a_row: 1
        cooldown: 1h
        notify: [oncall, pager]
      - name: slow-builds
        on:
          running_longer_than: 2h
          waited_longer_than: 15m
        notify: [oncall]
```

## Destinations

An address is a credential: a Slack incoming webhook authenticates by its URL, so whoever holds it can post to the channel. So the config holds where to find it and never the address, and billet refuses the address written in its place, without repeating it. It is read from the environment variable `url_env` names or the file `url_file` names, which must be readable only by its owner, when the control plane starts. So change one by restarting the control plane.

It must be `https`. Plain `http` is refused unless the host is a literal loopback address.

**A destination that cannot be read does not stop the control plane**, because scheduling matters more than a notice. It is logged as an error at the start, and every notice owed to it settles `failed` with the reason, which `billet alerts list` shows. To check one before a job goes red, run `billet alerts test --destination NAME`. It reads the address with your shell's environment and files, which may not be the service's.

A destination is delivered to while the config names it, even with no rule left, so what it is already owed still arrives. Notices owed to a destination the config no longer names are given up on when the control plane starts, with that reason, rather than owed for ever.

| `kind` | What is sent |
|---|---|
| `webhook` | a POST of the notice below |
| `slack` | a POST of `{"text": "..."}`, with `&`, `<` and `>` escaped so a job's name cannot mention a channel or make a link |

A webhook's notice is one JSON object, and a field is added to it, never renamed. `billet_alert` is the version, 1, and moves if one ever has to change:

```json
{"billet_alert": 1, "rule": "deploys", "condition": "failed", "repository": "acme/api",
 "workflow": ".github/workflows/deploy.yml", "job": "deploy", "branch": "main",
 "tier": "billet-8vcpu", "node": "epyc-1", "lease": "4f2a...", "run_id": 31,
 "run_url": "https://github.com/acme/api/actions/runs/31", "result": "failed",
 "disruption": "", "rerun": "", "seconds": 0, "fired_at": "2026-11-04T12:00:00Z"}
```

An empty string is not recorded. `condition` is one of `failed`, `cancelled`, `disrupted`, `running-long` and `waited-long`, and `seconds` is how long the job ran or waited for the last two. `rerun` is what [billet decided about re-running the job](status-and-leases.md#re-running-a-job-billet-ended) when the notice fired, which for a job that just ended is usually nothing yet.

## Rules

`on` names at least one condition, and each is judged apart:

| Condition | Fires when | What resets a streak |
|---|---|---|
| `failed` | GitHub reports the job as anything but succeeded or cancelled | the job's next run not failing |
| `cancelled` | GitHub reports the job cancelled | the job's next run not being cancelled |
| `disrupted` | billet recorded a [disruption](status-and-leases.md#what-leases-failures-is-and-is-not) against the job, at once, while it may still be running | a run ending undisrupted |
| `running_longer_than` | the job is still running past the duration, or ended after it | a run ending under it |
| `waited_longer_than` | the job started after waiting past the duration, from GitHub's queue time | a run that waited less |

A job GitHub cancelled before it started is not one of these. GitHub cancels and reassigns an assignment nobody ran, and that is nobody's failure. And `waited_longer_than` is judged once a job has started: a job still waiting has no row to judge, so use `billet_tier_waiting_seconds` for that. A job whose queue time GitHub's record never gave is not judged either way.

Each condition is judged, counted and cooled down apart: a failure that was not disrupted is a miss on `disrupted` and changes nothing about the failures' streak, and one job that both failed and was disrupted counts once on each. A rule's jobs are counted in the order their results arrived, so a success between two failures breaks the streak however the evaluation's passes fell.

`match` narrows a rule, and an empty one matches every job. Each field is a pattern, with `*` for any run of characters and `?` for one, `/` included:

| Field | What it is matched against |
|---|---|
| `repository` | `owner/name` |
| `workflow` | the workflow's file name, or its path for a pattern with a `/` in it. A job inside a reusable workflow carries the called workflow's file |
| `job` | the job's name |
| `tier` | the tier's label |
| `branch` | the run's own branch, read from GitHub |
| `step` | a step that ended the way the condition is about, read from GitHub. It takes only `failed` and `cancelled`, and a job that went red in another step counts as a run that did not hit |

**`branch` and `step` need the App to hold `actions: read`**, which `billet check` holds to. The branch is the one the queue-time sweep already recorded from GitHub when it has, and is read only for a run it has not. Steps are read once a run, for every job of it, and only from a job GitHub has concluded: a step still running has no conclusion yet. Reads take three a pass from each App installation and never its reserve, and a job waits for its turn. Once GitHub has been asked about a job, it waits an hour for an answer; if none comes, the job is judged neither way and the log says so, because a rule that silently stopped firing is what this exists to prevent. A job judged neither way breaks no streak.

A job whose runner billet lost has no failed step: GitHub leaves the running step without a conclusion (measured 2026-11-04, [Re-running disrupted jobs](../reference/records/job-reruns.md)). So a rule naming a `step` with `failed` does not fire for it; use `disrupted` for that.

## Noise

Everything below counts per rule and condition, and per job of a workflow of a repository, whichever tier or attempt ran it.

- **`in_a_row`** is how many runs in a row must hit before the rule fires, 1 to 100, default 1.
- **`cooldown`** is how long the rule then stays quiet about that job, default `1h`; `0s` is none. A hit during it still counts towards the streak.
- **A mute** silences a rule without a restart, and holds across a failover: `billet alerts mute deploys --for 2h`, and `billet alerts unmute deploys`. It has to end, within 30 days, because a rule nobody wants should leave the config. A muted rule's jobs go on being counted, and a hit it swallowed is not sent later.

A rule is about jobs that started after this deployment first saw it, so adding one does not page anybody about the last day.

## What it costs and what it promises

The evaluation and each destination run in the background of the control plane and can stop nothing. Every decision is written before anything is sent, so a destination that hangs holds no write and delays no job. Each verdict is one ledger write: a job that ended costs one per rule and condition that judged it, a success included, since a success is what ends a streak.

A job is judged once its result arrives, within six hours, and only if it started within the last seven days; a control plane down for longer than six hours does not judge what ended meanwhile. The evaluation reads on from the last job it decided, so a backlog is reached in order rather than hidden behind newer results.

One request goes to a destination at a time, each bounded at ten seconds, so one that hangs delays only its own queue. A redirect is refused, not followed, because a notice is not for wherever a destination sends it.

A notice gets five tries, thirty seconds apart from the end of the last and doubling. A 429, a 5xx or a failed connection is tried again; any other answer is not, since a Slack `invalid_token` will not change. A notice given up on is logged and shown by `billet alerts list`, and `billet_alert_notices{destination,state}` counts them, which is what to alert on for this itself.

Delivery is at least once: a notice delivered whose delivery could not be recorded is sent again.

No log line or stored error carries a destination's address or what it answered, only the status and the class of a failure. Nor does a refusal of the config: a name or a reference that is not a name is not repeated, in case it is the address pasted in the wrong place. A connection to a destination is used for one request and closed, so nothing it writes afterwards is read.

## Commands

| Command | |
|---|---|
| `billet alerts list` | each rule and its mute, the jobs each is counting, notices by destination, and those given up on |
| `billet alerts mute <rule> --for <duration>` | silence a rule until then |
| `billet alerts unmute <rule>` | lift it |
| `billet alerts test --destination <name>` | send one made-up notice, reading the address with this shell's environment and files |
