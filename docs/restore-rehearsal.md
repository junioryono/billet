# Rehearsing a restore

ADR-001 has said it since before backup existed: *"Backups: SQLite's backup API to S3, with a **rehearsed** restore. An untested backup is not one."*

`billet local backup` and `billet local restore` had unit tests, command tests, four rounds of adversarial review and a by-hand run. None of that is a rehearsal. What had never happened was the thing an operator will do on the worst day: take an archive off a real deployment, put it on a machine that has never seen it, start a control plane, and have a node connect.

Two legs now do that on every pull request. They are split because neither can do the other's half.

## The two legs

| | What it is | What only it can prove |
|---|---|---|
| `go test ./internal/e2e -run TestARestoredDeployment` | A deployment assembled through `internal/wiring`, exactly as `billet server` assembles one, backed up and restored into a directory that has never seen it | That the restored control plane **serves**: the same node, holding the certificate the ORIGINAL deployment issued and having enrolled with nothing, connects over the real mTLS wire and is accepted |
| `make restore-rehearsal` | The real `.deb` in a container that has never run billet, driving the real `billet local backup`, `restore` and `recover` | That a **packaged Linux host** can do it: the `billet` service account, the state directory's ownership and modes, and the config template an operator actually edits |

Both run in CI — the first with the ordinary test suite, the second in the `package-lifecycle` job — so this is a gate rather than a document, and it is cheap enough that no schedule is needed.

## What the second leg found on its first run

Every file a root-run restore published stayed root-owned inside a state directory the service account owns, and the restored control plane could not open one of them.

Three facts combined to make that invisible until somebody ran it. A restore on a packaged host runs as **root**, because the App key lands in root-owned `/etc/billet`. systemd's `StateDirectory=` repairs ownership recursively only when the **top** directory's owner is wrong — measured on systemd 255 — and after a restore it is already right. And `billet local up`, which the restore's own output named as the next step, repairs the five files a preflight `billet check` can create and no part of the authority: `ca/`, `ca.key`, `ca.crt` and `authority-created` stayed root's however many times it ran.

`billet local restore` now hands over what it wrote, and says so line by line. The set is derived from the plan it just executed rather than from a list kept beside the repair, because a list is exactly what was wrong before. Two entries are additions no action publishes: the `ca` directory itself — a root-owned 0700 one cannot even be traversed, so the authority under it is unreachable however its files are owned — and the two lock files the command created as a side effect of taking them.

## What neither leg proves

Said here rather than left to be assumed, because a gate that is believed to cover more than it does is worse than no gate.

- **Nothing starts a real control plane against a real GitHub App.** `billet server` cannot start without reaching GitHub — `server.Run` returns an error when `EnsureScaleSet` fails, and no config points the client elsewhere — so the serving proof is in-process and the packaged leg stops at `billet server --upgrade-probe`. That probe opens the ledger, migrates it, builds the allocator from the restored rows and reads the App key, which is everything a restore is responsible for and nothing after it.
- **The systemd units are not exercised.** No `systemctl start`, no `Type=notify` readiness, no `billet local up` (which refuses unless `billet check` reports GitHub *verified*). A container has no systemd.
- **No job runs.** The restored deployment is proved able to serve and to issue; nothing proves a workflow completes on it afterwards.

## And putting a deployment back over itself

The Linux leg goes one step further than a restore. It proves that `billet local restore` **refuses** the commissioned original — naming `billet local recover` as the operation that is right there — and then runs that recovery: the ledger is replaced, the one that was there is renamed to `billet.db.superseded-<taken-at>` rather than deleted, and `billet status` under the service account reports the deployment **sealed by an operator's seal**, which is the one `billet local up` does not clear.

That last assertion is not decoration. The seal a recovery takes before replacing the ledger lives *in* the ledger being replaced, and the archive's admission row is whatever it was when the backup was taken — open. A recovery that stopped there would hand back a control plane that takes new work immediately, while its nodes still hold compute the restored ledger has never heard of. The rehearsal is what found that.

## Off the disk it protects

The rehearsal above proves an archive restores. It says nothing about where the archive is, and `--out <dir>` puts it on the volume it protects.

`backup.s3` closes that, in both directions: a backup uploads what it just wrote, and `billet local restore --from-backup latest` fetches and restores on a machine holding nothing but the binary. The fetch is the half that matters — upload-only would leave an operator installing `aws-cli` during an outage — and it takes exactly the same path as a local restore once the archive is on disk, so there is one planner, one set of refusals and one executor rather than two that can disagree about what is safe.

What the round trip is tested against is a stand-in, not a real bucket: `internal/archivestore` drives a real HTTP round trip against an `httptest` S3 (the no-clobber header, the encryption header, the signature, the pagination, and that no operation can ever emit a `DELETE`), and `internal/deployarchive` proves the manifest is uploaded last, that an interrupted upload is not an archive anything will fetch, and that bytes which changed in the store are refused on the way in. **Nobody has yet run it against a real S3 or a real Ceph RGW.** That belongs in the manual procedure below.

## The manual rehearsal, for the acceptance account

**This has not been run.** It is written down so that when it is, it is run the same way twice, and its result belongs in `docs/aws-acceptance.md` beside the other measurements — as a record of what happened, never as a plan.

1. Stand up an isolated deployment in the acceptance account (`810711872940`, us-west-2) with its own state directory, its own GitHub App and its own ports, and run one real job on it.
2. `billet local backup --out <dir>` against the live control plane.
3. On a second host that has never seen it: `billet local restore --from <dir> --dry-run`, then with `--old-controller-fenced` after stopping **and disabling** the original everywhere.
4. `billet local up`, then a real workflow on the restored deployment, with the node reconnecting on its original certificate.
5. Repeat steps 2–4 through a real bucket — `backup.s3` on the source, `billet local restore --from-backup latest` on the target — and once more against a Ceph RGW endpoint, which is the path-style case the AWS one does not exercise.
6. Tear both down and record what was left behind.

Step 3's precondition is the one no tooling can establish: two authoritative controllers sharing one identity, one CA and one App credential diverge from the moment both are up.
