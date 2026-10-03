---
name: billet-controller-retirement
description: "Load when touching `billet server retire` (cmd/billet/serverretire*.go), internal/retirement (the journal, the completion and answer documents, the phase table), the controller_retirement ledger row, Inspector.AdmitOperations and the closed edge list in internal/lifeops, or the collection's retirement.yml, retirement-request.yml, retirement-settled-entry.yml and retire-answer.yml; or when a retirement holds, refuses, resumes or settles in a way you have to explain."
---

# Controller retirement

## What this area is

Retiring one controller of a PostgreSQL active-passive pair while the other, the survivor, keeps the deployment. The host either keeps its node (`retained-node`) or ran only the server and keeps no billet service (`server-only`, whose configuration is removed). The Ansible collection drives it, entering through `retirement.yml`. `billet server retire` (`cmd/billet/serverretire*.go`, dispatched from `cmdServer` before its flags) is the command every step calls. `internal/retirement` holds the journal, the completion and answer documents and the phase table. `controller_retirement` (migration 51) is the ledger row the two controllers contend for, written before any evidence is collected (`billet-state`). `Inspector.AdmitOperations` in `internal/lifeops` decides which service operations a retirement may perform. A retirement is reserved, declares intent, then moves the host through `stopped`, `archived`, `config-rewritten` and `node-restarted` to `done`. A tail then completes the row, clears the guard's marker and settles the journal.

Each invariant below is one line here and stated in full, with the incident or measurement behind it, in the reference file its group names. Read the reference whole before changing anything it covers: every rule here was paid for by a review round or a measured failure.

## Invariants

### The command: [references/command.md](references/command.md)

- **A retirement marks its guard, and a marked guard is released by nobody.**
- **The dry run is a CLASSIFIER that answers one word, and the role's caller (`retirement.yml`) branches on nothing else.**
- **The retirement caller requires a capable answerer before routing.**
- **The fresh retirement request keeps the installed variant.**
- **Retirement preparation and the endpoint move precede the request.**
- **Entry state separates retirement completion from retained ordinary work.**
- **Retained ordinary operations protect the retired controller, and binary changes use Go.**
- **A locator is not a validation, and discarding one converges over a live reservation.**
- **The transition after intent is one loop over the phase table, and every action is written behind the act it certifies.**
- **The tail is the local work first and the ledger last, and its order is what makes its crash windows recoverable.**

### The Ansible collection: [references/ansible.md](references/ansible.md)

- **The retirement caller enters through `retirement.yml` before both service-account imports, and its parser is `retire-answer.yml`.**
- **Retirement admits ordinary work from `route: ordinary`, confirmed cancellation or admitted settled retained entry, with one unverified check-mode preview exception.**
- **Retirement preparation does not evaluate desired configuration on an unchanged capable installation.**
- **A retained request requires its endpoint move before collection, and the retirement flag outlives frozen controller inventory.**
- **Retained ordinary work has two read-only admissions and a final closing proof.**
- **Retained work uses the shared node lifecycle and closes after every handler.**
- **A fresh reservation during cancellation is this run's cleanup obligation.**
- **Retirement CI selection has an execution inventory.**
- **R25/R42 execute main.yml at the namespace request boundary.**
- **R25/R42 observation must establish its scope.**
- **Fresh retained request callers have independent rendering oracles.**

### Service admission and stopped boundaries: [references/service-admission.md](references/service-admission.md)

- Retirement uses `Inspector.AdmitOperations` before intent, resumed mutation and every timer/controller stop and disable and retained-node enable, stop and start.
- Retirement admits a closed list of edges: each relationship property and its source and destination classes must match an entry in `internal/lifeops/operationclosed.go`.
- Retained inputs must survive the handoff: keep node configuration, identity, TLS certificates, keys, CA bundles and every other startup input on persistent storage, never under `/run`, `/tmp` or `/var/tmp`.
- Full effect admission precedes timer-stop metadata, stopped status, every phase journal write, archive rename and directory flush steps, and the configuration rewrite.
- **Admission granularity.** Admission cannot close a race against a concurrent root writer: however close an admission is to a mutation, a unit armed between them fires.
- **Retained retirement installs persistent inertness before any stop or disable.**
- **Retained inertness is effective loaded state, never a past condition result.**
- **Ordinary operation documents separate metadata from recursive work.**
- **The settled-entry systemd witness is a hybrid command test.**

## Checklists

**Any change here.** The phase table's every cell is written to `tests/fixtures/retirement/vectors.json` by the Go test and compared on every run, so the role's and the loader's readers are held to the Go table; regenerate it from the test, never by hand. The fixtures under `tests/fixtures/server-retire/` are the command's own (`TestTheServerRetireFixturesAreTheCommandsOwn`).

**A new retirement case in the collection.** `scripts/check-retirement-shards.py` owns the expected case counts: keep its counts, the sourced case files, `retirement-cases.md` and the CI matrix together ([references/ansible.md](references/ansible.md)).

**A new service operation or unit relationship.** It must match an entry in `internal/lifeops/operationclosed.go`, and the edge list is derived from billet's shipped units, the role templates and pinned systemd 255 sources ([references/service-admission.md](references/service-admission.md)).

## Related skills

`billet-releases-and-upgrades` (the converge guard and the upgrade root a retirement runs under), `billet-state` (the `controller_retirement` row and `state.Unreachable`), `billet-lifecycle` (the units and the inspector), `billet-infra-terraform-ansible` (the host role around the retirement files), `billet-identity-and-ca` (the authority the survivor's report is compared against).
