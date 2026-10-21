# ADR-018: The Git proxy answers as GitHub does

Accepted, 2026-10-21. Supersedes ADR-013's rule that the Git proxy serves only objects a ref reaches.

## Context

The node's Git proxy (`internal/node/gitproxy.go`) serves a guest's `git fetch` of a github.com repository from a bare mirror on the node, after GitHub has authorised the job's own header for that repository. Until this decision it served only objects a current ref of the mirror reaches, under protocol v0, so that an object a deleted branch once reached would never leave the mirror. It refreshed the mirror on the request path, under a per-repository lock that a request waited on without a bound, and every request to GitHub went through one shared HTTP/2 connection.

On 2026-10-21 that design failed a production fleet's CI in two ways.

- **`upload-pack: not our ref`.** actions/checkout fetches a pull request's merge commit by its name. When the pull request moved or was merged before the job ran, GitHub's merge ref no longer named that commit; the mirror still held it, no ref of the mirror reached it, and the mirror's upload-pack refused it. GitHub serves the same fetch. Dozens of jobs failed this way over five days.
- **Checkouts that hung.** One full-history checkout's lazy fetch of about 6,800 files ran for exactly ten minutes, the proxy's fixed transfer limit, and ended with `curl 18 transfer closed with outstanding read data remaining`. Two others ended with `HTTP 408` from GitHub after 85 and 130 seconds. Meanwhile every other checkout of the repository took 48 to 59 seconds instead of 8, and all of them recovered the second the ten-minute transfer was cut.

## What was measured

GitHub's git server, 2026-10-21, git 2.51.0 and hand-built smart-HTTP requests:

- **GitHub serves, by name, under v0 and v2, a commit no ref reaches.** Superseded `refs/pull/N/merge` commits 50 minutes, 2 days, 19 days and 77 days old were all served, and so was a commit that exists only in a fork, fetched through the parent repository's URL. A commit from another fork network was refused. `allow-reachable-sha1-in-want` is what GitHub advertises; `allow-any-sha1-in-want` is what it does.
- **A refusal is `200 OK` and one `ERR upload-pack: not our ref <name>` line, before anything else.**
- **GitHub's `408` is an idle limit on request bodies, not a total one.** A body trickled at one byte a second for 158 seconds was answered; a gap of 10.5 seconds or more between body bytes was answered `408` at about 10.2 seconds. A 14.5 MB body with 290,355 wants was answered.
- **`refs/pull/N/merge` moves only when the pull request's head is pushed,** within seconds of the push; it is not recomputed when the base branch moves. On the affected repository two of forty fifteen-second samples saw any ref change.
- **GitHub sends nothing before its response headers,** then side-band keepalives every five seconds while it counts a pack.
- **GitHub advertises no `packfile-uris`, `bundle-uri`, `object-info` or `ref-in-want`** under v2, and answers a request for one with an empty `200`.

git itself (v2.56.0's source, with experiments on a mirror holding a deleted branch's objects):

- Under v2, upload-pack serves any object present, whatever `uploadpack.allow*SHA1InWant`, `hideRefs` or a namespace say. Under v0 over HTTP it checks reachability for commits only, so a tree or a blob no ref reaches passes.
- In both protocols a `have` line for any present object is acknowledged, and haves seed delta bases: the protocol cannot keep an object a repository holds from a client that names it. git's own documentation says the only protection is not to hold it.
- Sixteen concurrent full fetches cost 34 s of CPU from 43 unconsolidated packs and 3.4 s from one pack with a bitmap.

## Decision

**A job GitHub has just authorised for a repository may be served any object that repository's mirror holds.** A mirror holds only what billet fetched from that repository on github.com, one mirror per trust class and repository, so it holds nothing GitHub would not give the same header: GitHub serves an object no ref reaches by its name, and a `have` can already find out whatever a repository holds. The rule billet had was stricter than GitHub, and the strictness was what failed jobs. Authorisation is unchanged: GitHub's own answer to the job's header, asked for every advertisement, standing for a minute, ended by a later refusal.

**Nothing that goes wrong in the mirror fails a fetch GitHub would answer.** A want or a shallow boundary the mirror lacks, a negotiation the proxy cannot read, a mirror whose upload-pack refuses or says nothing: each sends the request, whole, to GitHub. The first line of the mirror's answer is read before any byte reaches the client, so a refusal there still goes to GitHub.

**A refresh belongs to no request.** One refresh runs at a time per mirror, on the proxy's own context, with the header of the job that started it. An advertisement waits for it at most ten seconds and then sends GitHub's own advertisement; the fetch after that is still served from the mirror when the mirror holds what it names. A refresh that began before GitHub answered a request is joined and the refs compared again, so a job is never told a mirror's refs that are older than GitHub's answer to it. A lock file or a temporary pack found when a refresh starts was left by one that was killed, and is removed. Git's own automatic gc and maintenance are off, so no detached process outlives the refresh with the job's header in its environment.

**The proxy reaches GitHub over its own HTTP/1.1 connections.** Over the shared HTTP/2 connection every advertisement and every forwarded fetch on a node shared one connection's flow control, so one large download held back the rest, and a request body that stopped arriving for ten seconds was ended by GitHub with `408`. That is an inference from the symptoms; HTTP/1.1 removes the mechanism whether or not it was the cause. Every request body is read whole before GitHub sees a byte of it, so a guest that sends slowly never meets GitHub's idle limit.

**A git request ends when no byte has moved for three minutes,** and at two hours whatever happens. The fixed ten-minute limit of the other caches cut transfers that were still alive.

**Every git request ends with one line in the node's log:** where it was answered, why it was not answered from the mirror, its status, its bytes and how long it took. Every refresh logs its outcome and duration.

## What is not decided here

- **Pruning.** An object GitHub has since removed (a sensitive-data removal through GitHub Support) stays in a mirror until the mirror is reaped. The planned maintenance prunes objects no ref reaches after a retention window and adds `billet cache git forget` to drop a mirror at once.
- **Refreshing narrowly.** A refresh still fetches every ref, `refs/pull/*` included. Fetching only the refs that moved, with the default branch among the negotiation tips, and keeping the mirror in one pack with a bitmap, is the next change.
- **Protocol v2 to the guest.** The mirror still speaks v0 to guests; v2 would let the proxy forward GitHub's ref listing unchanged and serve only fetches.

## Rejected

- **Keep the reachability rule and send what it refuses to GitHub.** It is correct, but every full-history checkout's lazy fetch and every superseded merge commit would go to GitHub, and the rule protects nothing GitHub does not already hand out.
- **A checkout action that borrows objects from a mirror disk through alternates,** as several hosted runner services do. It keeps GitHub in every fetch and cannot refuse anything GitHub would serve, but it needs every workflow changed and a disk attached to every guest; billet's proxy needs neither.
- **Waiting for the refresh, however long.** It is what made one slow fetch every job's ten minutes.
