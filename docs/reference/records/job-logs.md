# Job logs

What the runner and the index actually did, for the facts [ADR-028](../decisions/adr-028-job-logs.md) relies on.

## What the runner writes

Measured on 2026-11-04 on the shared Linux test host: one real job of `billet-testbed/jobs` (run 41789737389, one `sleep` step) on an ephemeral runner, actions/runner 2.337.0, with every file the runner wrote kept by a hard link as it appeared, polled every 50 ms.

| Asked | Answer |
|---|---|
| Where step output goes | `_diag/pages/<timeline>_<record>_<n>.log`, and the same bytes in `_diag/blocks/<timeline>_<record>.<n>` |
| When a file appears | at its first write: the setup's and the job's at 07:04:44.96, the step's at 07:04:45.19, the cleanup's at 07:05:05.42 |
| Which records | one for each step that wrote, "Set up job" and "Complete job" among them, plus one for the job |
| What the job's record holds | every line again: 19 lines, exactly the other three files' 12, 6 and 1, each with its own time a few microseconds later |
| How the job's record is told apart | its id is a version 5 UUID (`ad86b988-2d51-5fef-8e47-d6501d04ad33`) and each step's a version 4. It appeared second, so order does not identify it |
| When a file goes | every file was gone by the job's end: both directories were empty |
| What a line looks like | a byte order mark first in the file, then `2026-11-04T07:04:44.9330649Z Current runner version: '2.337.0'`. A step's script keeps its terminal colours (`\x1b[36;1m`) |
| What the Worker log marks | `Processing step: DisplayName='Hold the runner'`, for the workflow's own steps, not "Set up job" or "Complete job" |

So the agent reads the step records and skips the job's, which gives it each line once and which step it was in; a runner that stopped using version 5 would cost duplicates within the job's budget, never a lost line. It holds each file open from the moment it appears, on an inotify watch since a step that writes little can be created and gone inside a poll, and reads through the unlink.

## Whether a line is masked

In actions/runner v2.338.1's source, `ExecutionContext.Write` calls `HostContext.SecretMasker.MaskSecrets` and only then `_logger.Write`, for both the step's logger and the job's, and `PagingLogger` writes what it is handed to these files.

Measured on 2026-11-04 on a real job on runner 2.337.0, with this branch's agent beside the runner and this branch's node service keeping what it sent. A step echoed a repository secret and a value it had masked with `::add-mask::`, and both were `***` in the kept lines: `secret-line *** end` and `masked-line *** end`. Neither value appeared anywhere in what the node kept. The secret also reads `***` in the step's own `env:` header.

## A step's name

Also measured on that job. The runner's marks in its Worker log have whole seconds, and four of the job's seven steps began in one second, in the same second as the runner's own setup, which is a page with no step and no mark. So a page is named after the first waiting mark of its own second, and the first page takes no mark at all. Naming each page after the latest mark at or before its first line put every name one step off.

A configured job-started hook is a step of its own, `Set up runner`, with its own mark and page. The hook's lines reach the job's log, `RUNNER_NAME` is set for it, and it runs under `bash --noprofile --norc -e -o pipefail`.

## Reading beside the writer

Also measured on that job. With the control plane's writer holding a day's file open in WAL mode, a search opening it `mode=ro` found the lines from the same account, as root and as `nobody`, an account that cannot write to the directory.

## Real JUnit files

Also measured on that job, through the agent and the node's service to the encoded test results. pytest's `--junitxml` and `gotestsum --junitfile` each gave a suite with the right counts, every case's status (`passed`, `failed`, `errored`, `skipped`) and every case's duration.

## What the index does

Measured on 2026-11-04 on the shared Linux test host: `modernc.org/sqlite` v1.56.0, which is SQLite 3.53.3, a file in WAL mode, 200,000 lines of about 95 bytes each.

| Asked | Answer |
|---|---|
| Does the driver have it | FTS5 with `tokenize='trigram'` and `content=''` both work |
| Insert | 5.8 s for the 200,000 lines, in one transaction: about 35,000 a second |
| Size | 58.4 MB for 19.1 MB of text, 3.06 times it, with the WAL checkpointed |
| A phrase, `"connection reset"` | found its one line in 1.6 ms, across both words |
| Case | `"CONNECTION RESET"` found the same line |
| Exclusion, `"error" NOT "timeout"` | found the one of two in 0.3 ms |
| A term under three characters, `"ab"` | found nothing and said nothing, which is why the query parser refuses one |
| Only an exclusion, `NOT "error"` | a syntax error, which is why the query parser refuses one |
| A range of rowids with a match | `rowid BETWEEN 100 AND 200` counted 101 |
