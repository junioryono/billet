# ADR-011: One control plane, several GitHub targets, and repository scope

## Status

Accepted. Implemented.

## Context

A deployment served exactly one GitHub organization. The `github:` block was the one credential, the scale-set client was built from `https://github.com/<org>`, and every tier hung off it. GitHub never shares self-hosted runners across owners, and a personal account cannot own runners at all (GitHub has repository, organization and enterprise scopes and no user scope), so a fleet that should also run the CI of a repository owned by a personal account, or of a second organization, needed a second deployment: a second control plane, a second state directory, a second CA, and a second set of hosts, because the packaging and the Ansible host role name `billet-server.service` and `billet-node.service` and one deployment per host. The motivating case is billet's own workflows running on a consumer's fleet, which is both the largest continuous acceptance test billet could have and impossible as things stood.

Two facts made repository scope cheap to add and expensive to get wrong. The vendored `actions/scaleset` client already parses a two-segment configuration URL as repository scope and registers at `/repos/{owner}/{repo}/actions/runners/registration-token`; billet refused the `/` in `github.org` precisely because of that. And the permission GitHub requires to register a repository's runners is the repository permission `administration: write`, which is far broader than the organization permission billet asks for, and billet refuses an App holding a permission it did not request.

## Decision

### A target is an organization or a repository, with one App credential each

A deployment declares one or more **targets**. `github:` stays valid and is the target named `default`; further targets are a `targets:` list, each `{name, org | repository, app_id, client_id, installation_id, private_key_path}`. The `github:` block is required whenever `targets:` is written and is always the first target: the archive, the identity store and the host role key that target by the name `default`, and a first target found by position would move its credential on a reorder or a rename. A `targets[]` entry named `default` is refused as a second spelling of the block, and two targets may not name one key file. Every tier belongs to exactly one target: `tiers[].target` defaults to the only target and is required when there are several, so adding a second target to a deployment is an explicit edit of every tier rather than a guess about which owner a label belongs to.

One control plane, one ledger, one node fleet, one CA and one deployment identity serve all of them. What is per target is the credential and what the credential reaches: one scale-set client, one message session per tier, one runner-group policy client for an organization, one App private key. A node never sees the difference; the plane resolves the tier's target when it mints a registration, and the trusted-runner-group check a node asks for carries the tier's label from wire version 21, so the plane can answer for the right owner on a deployment with several.

### A target's GitHub path is its identity on the wire and in the ledger

`owner` for an organization, `owner/name` for a repository. It is the configuration URL the client registers through and the value in the scale-set ledger's `org` column, so there is no migration: existing rows keep the organization, repository rows carry the path, and renaming a target in the config does not orphan a record. The target's `name` is a label for the operator and for the archive, never an identity GitHub sees.

### Repository targets are untrusted-only

A trusted tier is a non-default runner group GitHub restricts to an exact set of workflows, re-checked before every registration billet mints. A repository has no runner groups, so nothing on GitHub's side can restrict a pool there. Config refuses `trusted: true`, `runner_group`, `workflows` and `intercept` under a repository target with that reason, and the control plane refuses the same at startup through the target's client. A backend that admits only trusted work (docker) therefore cannot serve a repository target, and `billet init` says so rather than sending the operator to create a runner group GitHub has nowhere to put.

### The App permission set is chosen by scope, and repository scope is the wider grant

An organization target's App holds `metadata: read` and `organization_self_hosted_runners: write`. A repository target's App holds `metadata: read` and `administration: write`, because that is the only permission GitHub offers for registering a repository's runners. `billet github-app create --repository` discloses it as the wider grant it is, and states what billet never uses it for: the repository's settings, its collaborators, its branch protection. The installation is verified against the set for its scope, in both directions, so an organization App cannot be pointed at a repository or the reverse. Installing on **only the selected repository** rather than every repository the account owns is the operator's choice in GitHub's installation form, and the getting-started text says to make it.

### The manifest form follows the owner's account type

A personal account's App is created at `https://github.com/settings/apps/new`; an organization's at `/organizations/{org}/settings/apps/new`. `github-app create` resolves the owner's type through the public `GET /users/{owner}` before the browser opens, because a lookup that fails costs nothing while a manifest posted to the wrong form costs an App.

### Personal accounts are served one repository at a time

There is no `owner/*` expander. It would need `metadata: read` on every repository the account owns and would register runners on repositories nobody asked for. A target names one repository; a second repository is a second target with its own App.

### The archive, the identity store and the host role carry one key per target

`billet local backup` captures every target's App key under the all-or-none rule (archive schema 3; a schema 2 archive reads as the single target `default`), and restore and recover install each. A store-backed identity keeps the `default` target's key at the parameter it always had and a further target's at `github-app-key-<name>`. The Ansible host role installs each further target's key at `/etc/billet/app-private-key-<name>.pem` under the same owned-versus-foreign rule as the first, from `billet_github_target_key_srcs`.

## Measured

Against a private repository under a personal account on 2026-09-04, through `billet github-app create --repository`, `billet check`, the opt-in `TestLiveRepositoryScope` and `billet server --dry-run`:

- The manifest flow landed on the personal-account form (`/settings/apps/manifest`), created the App, and installed it on the one selected repository; the installation reported account type `User` with exactly `administration: write` and `metadata: read`.
- The Actions service answers the vendored client's runner-group lookup at repository scope: `_apis/runtime/runnergroups/?groupName=default` returned one group, id 1, named `Default`, `isDefaultGroup: true`. So `EnsureScaleSet`, `Describe` and `DeleteScaleSet` need no repository branch; the one rule added is that a tier naming any other group on a repository target is refused before the service is asked.
- The scale set was created in that group, described with its label, a message session opened and closed with zeroed statistics, and the set deleted and proved absent. `billet server --dry-run` registered through `/repos/{owner}/{repo}/actions/runners/registration-token`, created the set and long-polled its session; `billet teardown --all` deleted it.

Nothing has yet run a job on a repository-scoped tier of a real fleet; that is Stage 6 of the issue and belongs to the consumer's rollout.

## Consequences

- An existing config is unchanged: `github:` is the `default` target and a single tier needs no `target`. Adding a target is additive.
- The node wire moves to version 21 for the tier on the trusted-runner-group request; an older node on a one-target plane is answered as before, and on a several-target plane its untargeted request is refused naming the version.
- A repository target's App is a broader credential than an organization's, held on the same host as the others. The trade is stated at creation and in the concepts pages; there is no narrower permission to ask for.
- `billet init --repository` cannot generate a docker trial, and says why; docker stays the organization-only backend.

## Alternatives considered

- **Repository scope plus a second deployment per target.** Rejected: it needs a second control plane and a second node host per extra target, or templated systemd units across packaging, lifecycle, backup and the role, which is as large as multi-target and worth less, since the second deployment cannot share the fleet.
- **A per-target column on the scale-set record.** Rejected: the GitHub path already identifies the owner, and a name that can be edited in config is the wrong key for a record about a remote object.
- **Resolving the trusted-runner-group request by group name alone on an old node.** Kept only where it is unambiguous: a one-target plane. Two organizations can each have a group of the same name, so on several targets an untargeted request is refused rather than guessed.
- **An `owner/*` target for a personal account.** Rejected, above.
