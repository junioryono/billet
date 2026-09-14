# Retirement caller coverage

Section R (`BILLET_GATE_ONLY=retirement`) sources `retirement-cases.sh`. Command answers come from the committed producer corpus; the route harness runs real preparation and retirement tasks, with an ordinary-boundary sentinel. The separate boundary case runs the real `main.yml` twice in separate processes in one namespace. These cases and this fix pass were reviewed by reading only; no build, test, lint or formatter was run.

| Contract | Case and assertion |
|---|---|
| Ordinary | R1 admits the sentinel after one classifier call and no retirement mutation or recovery. The ordering gate holds both service-account imports inside the ordinary block. |
| Continuation | R3 uses each committed continuation classifier, including an unreadable ledger and a disagreeing row; checks self-only stdin, the recorded survivor, no `--installed-sha256` in billet argv, and no other-host billet command. It does not observe Ansible-side configuration reads or digest computation. |
| Done | R4 consumes `done-unchanged`; R5 consumes `retired-pending`, reports the obligation and calls no survivor. The pending handoff case checks completion → acknowledgement → continuation, both carried documents and installed environment forwarding. |
| Capability | R8 covers old releases beside installed controllers, unknown installations, node-only and never-commissioned evidence, both server policies and both request flags; each fixed artefact veto; absent answerers (including controller and unknown installation under both server policies); `v0.10.1` as an explicit incapable maintenance release; merge/cyclic-merge, false/sequence/null/integer locator, undecodable/duplicate and explicit-state-without-locator holds, plus omitted-locator (absent or null state), empty-mapping and null-node holds, a named node-only mapping and a YAML-decodable package seed; missing, unknown and unreadable version types; development builds; and attempted classification with no JSON. `executable_version_check.py` covers the first-line grammar, snapshot build-suffix refusal, Go whitespace boundaries, timestamp validity and unsuccessful exits beside output. `retirement_config_check.py` covers forbidden anchors, aliases, merges and tags (including nested occurrences), duplicates, UTF-8, single-document and mapping requirements, locator types and padding, identity_dir precedence, state-block inheritance, account-dependent default holds, the actual package seed, absence and node-mapping requirements. Every hold asserts its named reason. |
| Check mode | R9 asserts the reported route for all seven route words, one read-only classifier call, no retirement mutation or recovery, ordinary admitted alone. Incapable, absent and unreadable answerers and an attempted-but-unanswered classifier each report hold and succeed without ordinary work. |
| Caller parser failure | `r7-caller-unknown-route` feeds a corrupted case-local answer through `retirement.yml`, asserts the named route refusal and reported hold, and proves no ordinary sentinel, ordinary task, mutation or recovery runs. `r7-caller-unknown-state` checks that a validated hold keeps the classifier's explanation. |
| Unsupported variant | R10 uses the producer's installed-both fresh-request fixture beside a server-only rendering and proves bypass. |
| Unexplained marker | The `dry-run-adopt` cases hold with and without a request. |
| Unreadable row | R15 uses the commissioned and damaged-identity hold fixtures with and without a request. |
| Binary recovery | R21 runs the existing recovery tasks for guard and legacy claims, clears the fence and claim/pointer, runs no ordinary task, and fails at `Require a fresh converge`. A missing input propagates the recovery task's own refusal before any stop. A separate second Ansible process classifies afresh. R9 covers recovery preview. |
| Real entry | `r-boundary` has no installed configuration or identity directory and consumes the settled done answer through real `main.yml`. No server directory, configuration, unit, timer or mount appears; the second process has an unchanged clean recap. |

# Corpus gaps

The existing continuation classifier fixtures all report `intent`; there is no classifier fixture for each later phase or for a journal already at `done`. Thus R3 cannot prove every phase and R4/R5 and the boundary prove the done ANSWER and bypass, not a done-shaped classifier observation. There is no settled `unchanged` answer with `postconditions.status: republished`, so the changed-reporting branch has no positive fixture control.

R10 has no retained-node journal classifier at intent, after the archive or done. R11 has no unexplained status-only or stage-only classifier; the incapable-answerer screen does exercise those path vetoes, and the marker classifier is covered. R14 has no another-host row classifier for either request flag. R15 has no node-only or never-commissioned unreadable-row classifier; those admissions are covered only in the role's incapable-answerer path. R18 has no abnormal-claim classifier fixture: substituting the unexplained-marker answer after damaging a claim would prove a different refusal, so that case was not fabricated. R8 still lacks a settled-journal/closed-status pair from the producer corpus; its three independent path vetoes do not establish that paired state. No file under `tests/fixtures/` was written, edited or staged.

The two inventory transports in the tail case share one namespace and its real prepared guard; this proves caller sequencing and document binding, not independent machine custody. Fleet preparation and the remaining reservation cases belong to 5c.d.

# Mutations

| Mutation | Case that must reject it |
|---|---|
| Branch on dispatch rather than route | R15's hold fixtures have request-like/unknown-ledger dispatches; no ordinary sentinel may run. |
| Default an unknown route to ordinary | `r7-caller-unknown-route` drives the caller and requires a reported hold and a closed ordinary boundary. The original R7 cases prove parser rejection only. |
| Skip the incapable artefact screen | `r8-artefact-journal.json`, `r8-artefact-authority-status`, `r8-artefact-config-serverless.yaml`. |
| Gate compatibility on desired server policy | R8's controller/unknown holds under false and node/fresh admissions under true. |
| Ignore requested on the incapable path | R8's requested node/fresh/empty/unreadable cases. |
| Collect fleet evidence through billet or pass an installed digest on continuation | R3's all-host call scan and argv/stdin assertions. |
| Omit completion or the post-acknowledgement continuation | `r5-handoff` requires the exact five retirement calls and both handoff documents. |
| Enter ordinary work on server-only done | `r-boundary`, through real main, plus its second-process recap and absent-path assertions. |
| Treat recovery as ordinary | R21's guard and legacy cases require finalization, the fresh-converge refusal, and no ordinary task. |
| Continue after recovery or reclassify in one invocation | R21's first invocation must fail at the existing boundary after exactly one classification. |
| Map legacy recovery to hold | `r21-legacy` must finalize and reach the fresh-converge refusal. |

The design's R16, R17, R19 and R13 mutations concern new-request preconditions, reservation freshness, rescue and unreachable collection in 5c.d. Cancellation also remains there: this revision reports it in check mode and holds it in a real run. The pending-row caller is already present here, including its strict survivor guard predicate; its independent survivor-admission cases remain with R16.

# Contract notes

The complete-classifier floor is `v0.11.0`, the next minor cut from `main`; `v0.10.1` remains an incapable maintenance-release control. The caller accepts one UTF-8 YAML mapping with string mapping keys, no anchors, aliases, merge keys, explicit tags or duplicate keys anywhere, and non-empty absolute string locators without padding. It validates both supplied locators before selecting identity_dir over state_dir. A null state block is absent; a non-null state block supplies no default, but inherits a supplied state_dir just as applyStateDefaults does before semantic validation. The caller uses the packaged identity path only for a positively absent configuration; undecodable bytes or an unestablished locator hold by name. A server with neither locator and no state block (or a null state) holds because Go's defaultStateDir depends on the running account: it uses os.UserConfigDir and falls back to .billet/server if that lookup fails. Only an absent server key beside a node mapping establishes node-only; a configuration with neither section or a non-mapping node holds by name. This deliberately tightens revision 12's undecodable-config fallback in the caller; the Go classifier's accepted custom-locator residual is unchanged. The Go classifier's recovery reason says to retry classification, while revision 12's explicit caller boundary requires a fresh converge: this implementation ends the invocation and lets the next preparation/classification do that retry. The older `dry-run-request`, `dry-run-adopt` and `dry-run-unknown-ledger` filenames describe dispatch, not route; the command's route wins. No command behaviour was changed.
