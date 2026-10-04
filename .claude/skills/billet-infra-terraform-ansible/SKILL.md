---
name: billet-infra-terraform-ansible
description: "Load when touching any .tf, .tfvars or classification.json, internal/tfclass, internal/tfpolicy, the IAM billet generates, any file under ansible_collections/ (the host, ssh_access, connector, macos_node and development_host roles, the fleet playbook), or actions/converge-fleet; or when a converge or an apply touches a host that is running work."
---

# Terraform and Ansible

## What this area is

Three layers own three things. Terraform creates cloud resources and returns narrow outputs; Ansible converges existing machines over SSH (packages, users, units, Firecracker, Ceph, bridges, the config, the upgrade transaction); billet owns live jobs, leases, identity, custody and drains. `docs/reference/decisions/adr-004-terraform-provider.md` defers a Terraform provider until billet has a configuration API, so `terraform/modules/billet` is an infrastructure module, and enrollment stays a human fingerprint comparison outside `terraform apply`. `ansible_collections/junioryono/billet` holds the `host` role (a Linux control plane and Firecracker compute host), `ssh_access` (the keys a converge connects with and sshd hardened only once they leave a way in), `cloudflared_connector` and `warp_connector` (the host halves of Routes C and D below) and `development_host` (a developer machine beside the runners), with examples under `examples/single-host-docker` and `examples/firecracker-host`.

Each invariant below is one line here and stated in full, with the incident or measurement behind it, in the reference file its group names. Read the reference before changing anything the invariant covers. Controller retirement in the collection is its own skill, `billet-controller-retirement`.

## Invariants

### Terraform: [references/terraform.md](references/terraform.md)

- **The terraform gates are outside `make check` and CI runs them on every PR that touches more than documentation.** Run `make tf-fmt-check tf-validate tf-test tf-lint tf-scan` before pushing a `.tf` change.
- **The root `billet init hybrid` writes is validated by terraform, not by Go.**
- **`tfclass` says what a change costs a running deployment; `terraform plan` cannot.**
- **`tfpolicy` keeps the module's IAM equal to the generator's.**
- **Modules pin to release tags, and every layer names the same version.**
- **What each module creates.**
- **`terraform destroy` refuses under a running CodeBuild build, and it is the module that refuses.**

### The host role: [references/host-role.md](references/host-role.md)

- **The host role owns the fail-closed ledger mount and the upgrade transaction.**
- **The role gives way to a rollout, and renders the timers that carry one out.**
- **The role keeps needrestart away from billet's services.**
- **A converge restarts services, so never drive one from a runner billet manages.**
- **The ordinary node restart is requested, never awaited.**
- **The role's variables that must be supplied, never guessed.**
- **The host role migrates a node's endpoint through billet's own commands, and infers nothing from the configurations.**
- **The scenario tests are make targets, each one converging the role in a specific state.**
- **The collection's `galaxy.yml` carries the release version.**
- **Every JSON a role consumes has fixtures written by the Go test that produces it.**

### Reaching the fleet: [references/fleet-and-connectors.md](references/fleet-and-connectors.md)

- **Reaching hosts is configuration management, not scheduling.**
- **The shipped playbook `junioryono.billet.fleet` is the consumer's `site.yml`, and it runs the converge guard first in every play.**
- **A Mac's configuration is converged by `macos_node`, as the node account, and a change is a drain.**
- **The connector roles read a bearer token per host from the environment and refuse what a healthy-looking mistake would hide.**
- **`actions/converge-fleet` is the consumer's one pin, and the collection it runs is the checkout it runs from.**
- **`ssh_access` never hardens a host its own converge leaves with no way in, and that refusal is not gated.**
- **The guest resolver refuses private answers and caps its cache at five minutes.**
- **The Ceph health alert mails a problem once, and nothing else.**

### The preparation dry run: [references/preparation-dry-run.md](references/preparation-dry-run.md)

- **A dry run of the preparation is answered by the executable a converge would be answered by.**
- **A dry run compares across a pending daemon-reload and refuses only billet-owned policy that disagrees or cannot be compared.**

Measured facts for this area, dated: [references/measured-facts.md](references/measured-facts.md).

## Checklists

**A `.tf` change.** Run `make tf-fmt-check tf-validate tf-test tf-lint tf-scan` (`make tools` installs the pins). A change to a resource's cost to a running deployment is classified by `tfclass`; an IAM change keeps `tfpolicy`'s drift test green.

**A role change.** Find the scenario target that converges the role in the state you changed ([references/host-role.md](references/host-role.md)) and run it. A JSON the role reads is regenerated by the Go test that produces it, never edited.

## Where the tests are

- `actions/convergefleet_test.go`: the action's scripts against fakes on PATH (`ansible-playbook`, `ansible`, `ansible-galaxy`, `nc`, `warp-cli`, `ssh-keyscan`, `sleep`), every refusal by its message and by what was not called, and the pin files against `ci.yml`.
- `internal/tfclass/*_test.go` (`make tf-classify`), `internal/tfpolicy` (the drift test), `scripts/codebuild_destroy_guard_test.go`, `terraform/modules/*/tests/*.tftest.hcl`, `terraform/modules/billet/modules/fleet-ec2/lambda/*_test.py`.
- `ansible_collections/junioryono/billet/tests/*` (the scenario playbooks and shell checks), `scripts/check-module-sources.sh`.

## Related skills

`billet-providers-aws` (the IAM and the CodeBuild facts the modules encode), `billet-releases-and-upgrades` (the host transaction and version pinning), `billet-config` (`--emit ansible`), `billet-lifecycle` (the units the role renders). Also `billet-controller-retirement` (retirement in the collection).
