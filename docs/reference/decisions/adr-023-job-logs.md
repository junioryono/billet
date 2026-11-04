# ADR-023: A job's log is kept on its node and indexed on the control plane

Accepted, 2026-11-04. Issue #534.

## Context

A job's log lives at GitHub, one job at a time, for as long as the run is retained. Finding when an error first appeared, or which jobs hit one failure this week, means opening runs one by one. billet already has a guest agent reporting to the node, and a ledger that knows every job's repository, workflow, branch, tier and node.

Three things bound the design. The ledger's single writer slot must stay free for scheduling, and the control plane reads the whole ledger at every start, so logs cannot live there. A log is what the job wrote: untrusted, as sensitive as its workflow makes it, and never a billet decision. And the runner's own files, measured on a real job ([the record](../records/job-logs.md)), are deleted as soon as they are uploaded, so they must be opened as they appear.

## Decision

**The text stays on the node that ran the job.** The agent in the job's VM reads the runner's step logs, each chunk of a step's lines is sent over the guest-facing service the agent already reports through, and the node stores it compressed under `<node.state_dir>/joblogs/<lease>/`. A job's log is bounded per job and per node, and evicted a whole job at a time, oldest first, past `node.logs`. Nothing is kept unless a tier sets `tiers[].logs.keep`, which only a Linux Firecracker tier can, because that is the guest that runs the agent.

**The control plane holds an index, not the text.** A node ships each kept chunk over the node wire, and the control plane tokenises its lines into a contentless FTS5 table using the trigram tokenizer, which stores what matches and not what was said. A search finds line numbers in the index and reads their text back from the node, through the node wire. The cost is that a line whose node is gone cannot be shown, and the search says so on that line, since a line it cannot read is never a line that does not exist.

**One SQLite file a day, in a directory of its own, on every deployment.** A retention and a size bound then remove whole files, which costs no delete, no tombstones and no vacuum, and a search opens only the days in its window. It is not the ledger and shares nothing with it, and losing all of it loses no decision: it is a cache of what the nodes hold.

**An index has an identity, and a node re-ships to one it has not reached.** Each registration's answer names the index. A promoted controller, a restored one or a deleted directory has a new one, and every node then ships all it still holds. Until they have, a search on that controller finds fewer lines than the fleet holds, and its record says when the index began.

**Reading is the operator's.** `billet logs search` runs on the control plane's host. The index is read from its files. Text comes through the operator socket, a Unix socket under the identity directory that serves root and the control plane's own user, by what the kernel says of the peer, which adds no listener that #28's rule would have to answer for. Who may read whose logs per target is not decided here: nobody but an operator can read any, and a per-target reader model waits for the first listener that would need it.

**What the agent reads has been through GitHub's secret masking.** The runner masks a line before it logs it, so the files the agent reads hold `***` where GitHub's own log does. Nothing else a job writes to disk has been through it, which is why these files are read and nothing else of a job's. The source says so ([the record](../records/job-logs.md)); a live check is listed there as owed before this is relied on.

## Rejected

- **In the ledger:** indexing a busy job would take the single writer slot from scheduling, and the startup integrity scan reads every byte of it.
- **Everything on the controller:** simpler to read and survives a node's loss, but the controller's disk and the node wire then carry every byte for as long as it is kept, on a host that is otherwise small by design (ADR-001).
- **An external store:** a dependency a self-hosted deployment does not otherwise need, for a feature most would then not turn on.
- **`pg_trgm` on a PostgreSQL deployment:** it needs an extension privilege and stores the text, which is what keeping the text on the nodes avoids, and it would be a second implementation to keep right.
- **GitHub's REST log after the job:** needs no code in the guest and covers EC2 and CodeBuild, but costs the shared installation budget a job at a time, gets nothing while a job runs, and gets nothing once GitHub's retention ends.

## Consequences

- Search reaches back as far as the nodes kept the text and the index kept the day, whichever is shorter.
- A node's disk carries the logs of what it ran; `billet check` does not yet report how full that is.
- Only an image whose agent is new enough sends logs. An older image's job has none, and its report is unaffected.
- The trigram index is about three times the text it covers, and it cannot find a term of fewer than three bytes, which the query parser refuses rather than answering nothing.
