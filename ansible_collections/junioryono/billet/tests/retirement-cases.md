# Retirement caller coverage

Section R (`BILLET_GATE_ONLY=retirement`) sources `retirement-cases.sh`. Command answers come from the committed producer corpus; the route harness runs real preparation and retirement tasks, with an ordinary-boundary sentinel. The separate boundary case runs the real `main.yml` twice in separate processes in one namespace. These cases and this fix pass were reviewed by reading only; no build, test, lint or formatter was run.

| Contract | Case and assertion |
|---|---|
| Ordinary | R1 admits the sentinel after one classifier call and no retirement mutation or recovery. The ordering gate holds both service-account imports inside the ordinary block. |
| Continuation | R3 uses each committed continuation classifier, including an unreadable ledger and a disagreeing row; checks self-only stdin, the recorded survivor, no `--installed-sha256` in billet argv, and no other-host billet command. It does not observe Ansible-side configuration reads or digest computation. |
| Done | R4 consumes `done-unchanged`; R5 consumes `retired-pending`, reports the obligation and calls no survivor. The pending handoff case checks completion → acknowledgement → continuation, both carried documents and installed environment forwarding. |
| Capability | R8 has 48 hold cases: `below-floor` (`v0.10.1`), `unreadable` and `absent` answerers × `billet_enable_server` true/false × requested true/false × normal/check mode × node-only bytes/fresh-host shape. Every case asserts the reported hold names `v0.11.0` and the upgrade-or-older-collection remedy, no classifier call, no recovery, no ordinary tasks and no ordinary sentinel. Requested cases additionally assert the named `--requested` refusal. Normal mode fails; check mode succeeds. Missing and unknown version types also hold; development builds classify; attempted classification with no JSON holds. `executable_version_check.py` retains its first-line grammar, snapshot build-suffix refusal, Go whitespace boundaries, timestamp validity and unsuccessful-exit coverage. |
| Check mode | R9 asserts the reported route for all seven route words, one read-only classifier call, no retirement mutation or recovery, ordinary admitted alone. Incapable, absent and unreadable answerers and an attempted-but-unanswered classifier each report hold and succeed without ordinary work. |
| Caller parser failure | `r7-caller-unknown-route` feeds a corrupted case-local answer through `retirement.yml`, asserts the named route refusal and reported hold, and proves no ordinary sentinel, ordinary task, mutation or recovery runs. `r7-caller-unknown-state` checks that a validated hold keeps the classifier's explanation. |
| Unsupported variant | R10 uses the producer's installed-both fresh-request fixture beside a server-only rendering and proves bypass. |
| Unexplained marker | The `dry-run-adopt` cases hold with and without a request. |
| Unreadable row | R15 uses the commissioned and damaged-identity hold fixtures with and without a request. |
| Binary recovery | R21 runs the existing recovery tasks for guard and legacy claims, clears the fence and claim/pointer, runs no ordinary task, and fails at `Require a fresh converge`. A missing input propagates the recovery task's own refusal before any stop. A separate second Ansible process classifies afresh. R9 covers recovery preview. Legacy ordinary and continuation cases retain ordinary admission and the named hold before retirement mutations, respectively. |
| Real entry | `r-boundary` has no installed configuration or identity directory and consumes the settled done answer through real `main.yml`. No server directory, configuration, unit, timer or mount appears; the second process has an unchanged clean recap. |

# Corpus gaps

The existing continuation classifier fixtures all report `intent`; there is no classifier fixture for each later phase or for a journal already at `done`. Thus R3 cannot prove every phase and R4/R5 and the boundary prove the done ANSWER and bypass, not a done-shaped classifier observation. There is no settled `unchanged` answer with `postconditions.status: republished`, so the changed-reporting branch has no positive fixture control.

R10 has no retained-node journal classifier at intent, after the archive or done. R11 has no unexplained status-only or stage-only classifier; the marker classifier is covered. R14 has no another-host row classifier for either request flag. R15 has no node-only or never-commissioned unreadable-row classifier; the capable command's admissions remain outside the caller's corpus coverage. R18 has no abnormal-claim classifier fixture: substituting the unexplained-marker answer after damaging a claim would prove a different refusal, so that case was not fabricated. The incapable-answerer path holds without reading artefacts, so it needs no settled-journal/closed-status fixture pair. No file under `tests/fixtures/` was written, edited or staged.

The two inventory transports in the tail case share one namespace and its real prepared guard; this proves caller sequencing and document binding, not independent machine custody. Fleet preparation and the remaining reservation cases belong to 5c.d.

# Mutations

| Mutation | Case that must reject it |
|---|---|
| Branch on dispatch rather than route | R15's hold fixtures have request-like/unknown-ledger dispatches; no ordinary sentinel may run. |
| Default an unknown route to ordinary | `r7-caller-unknown-route` drives the caller and requires a reported hold and a closed ordinary boundary. The original R7 cases prove parser rejection only. |
| Admit an incapable or absent answerer from local evidence | R8's node-only and fresh-host shapes both hold without ordinary tasks or the sentinel. |
| Gate compatibility on desired server policy | Every R8 answerer state holds under both server policies. |
| Ignore requested on the incapable path | Every requested R8 case requires the named `--requested` refusal beside the floor reason. |
| Fail a compatibility hold in check mode or admit ordinary work | Every check-mode R8 case succeeds while reporting hold and keeping the sentinel bypassed. |
| Collect fleet evidence through billet or pass an installed digest on continuation | R3's all-host call scan and argv/stdin assertions. |
| Omit completion or the post-acknowledgement continuation | `r5-handoff` requires the exact five retirement calls and both handoff documents. |
| Enter ordinary work on server-only done | `r-boundary`, through real main, plus its second-process recap and absent-path assertions. |
| Treat recovery as ordinary | R21's guard and legacy cases require finalization, the fresh-converge refusal, and no ordinary task. |
| Continue after recovery or reclassify in one invocation | R21's first invocation must fail at the existing boundary after exactly one classification. |
| Map legacy recovery to hold | `r21-legacy` must finalize and reach the fresh-converge refusal. |

The design's R16, R17, R19 and R13 mutations concern new-request preconditions, reservation freshness, rescue and unreachable collection in 5c.d. Cancellation also remains there: this revision reports it in check mode and holds it in a real run. The pending-row caller is already present here, including its strict survivor guard predicate; its independent survivor-admission cases remain with R16.

# Contract notes

The complete-classifier floor is `v0.11.0`; `v0.10.1` remains an incapable maintenance-release control. An incapable or absent retirement answerer always holds, under either value of `billet_enable_server` and with or without `--requested`. The reason names `billet_retire_capability_floor` (`v0.11.0`): this collection needs billet at or above that floor on the host; upgrade the managed binary or converge with an older collection. Requested retirement is also refused by name. Check mode reports the hold and succeeds without ordinary work. Local configuration, a clean identity locator, absent retirement artefacts and one stopped or absent service unit cannot prove that no controller or shared reservation exists: a controller may run by hand, under another supervisor, or as a surviving child of a stopped `KillMode=process` unit. The caller therefore reads none of those as compatibility evidence and keeps no three-path artefact screen.

A legacy claim keeps its own rule: a verified capable executable classifies read-only; `ordinary` enters ordinary convergence, `recovery` runs the existing recovery alone and ends at the fresh-converge boundary, and every other route holds. Without a capable executable it holds. Preparation leaves the answerer empty for a legacy claim, a pre-guard managed binary with no candidate, or a host with no billet and no candidate; the last case also lacks the binary source required by fresh-host validation. An install or binary change stages a candidate, but candidate-first preparation alone selects it as answerer. A managed binary that already answers the guard remains the answerer even after staging a newer candidate, so a below-floor managed answerer still holds in that case. The floor concerns the selected executable's observed version, never the desired installation pin; recognised development builds attempt strict classification.

The Go classifier's recovery reason says to retry classification, while the caller boundary requires a fresh converge: this implementation ends the invocation and lets the next preparation/classification do that retry. The older `dry-run-request`, `dry-run-adopt` and `dry-run-unknown-ledger` filenames describe dispatch, not route; the command's route wins. No command behaviour was changed.
