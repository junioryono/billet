---
name: billet-shell-gates
description: "Load before editing any .sh file, any Go that emits shell, or any pipeline whose exit status is a verdict: scripts/*.sh, the guest image and kernel builds, the EC2 provisioning script and AMI verifier, the CodeBuild buildspec, the toolcache installers, the guest launcher; and when a build passed a check it should have failed, or a gate refused a correct artifact."
---

# Shell that has to be able to fail

## What this area is

billet's shell lives in `scripts/` (guest image, guest kernel, release, install, rehearsals), in `internal/runnerimages/install-toolcache.sh` (sourced by both the guest build and the EC2 builder), and in Go that renders shell: `internal/provider/ec2/build.go` (the provisioning script), `internal/provider/codebuild/buildspec.go`, the tart and firecracker guest launchers. Every one of them contains gates whose whole purpose is to end a build, and every rule below is a way one of those gates computed the right answer and then let the build succeed.

Each invariant below is one line here and stated in full, with the incident or measurement behind it, in the reference file its group names. Read the reference before changing anything the invariant covers.

## Invariants

### Exit statuses and the shells' traps: [references/exit-status.md](references/exit-status.md)

- **`set -e` is ignored for a pipeline that begins with `!`.**
- **A gate in the middle of a `;` chain is not a gate.**
- **The harness that executes generated shell must withhold any ambient setting the gate must hold without.**
- **`grep -q` under `pipefail` returns the writer's SIGPIPE.**
- **grep's status is three-valued.**
- **A gate's verdict is its exit status, captured before anything filters the output.**
- **Diagnostics that pipe into `head` fail a passing gate.**
- **`grep -Ff anchor.crt bundle` matches any certificate.**
- **`env` resolves executables, not builtins.**
- **`pgrep -f <marker>` matches its own invoking shell.**
- **A substring of `env`'s output is not a variable.**
- **`stat -f` is the filesystem form on GNU coreutils, so a BSD-first mode read never falls back on Linux.**
- **A prefix assignment on a shell FUNCTION call persists after the call in dash and zsh.**
- **A recap is parsed from the recap rows alone, and the pass's status is captured under `pipefail`.**

### Builders, user data and installers: [references/builds-and-installers.md](references/builds-and-installers.md)

- **A probe on the development Mac is not evidence about the builder.**
- **EC2 user data is 16384 bytes and base64 is the wrong way to spend it.**
- **The toolcache installers are one file with one seam.**
- **Every toolcache vendor spells the architecture differently, and the record has three states.**
- **Vendored scripts are pinned to LF.**

Measured facts for this area, dated: [references/measured-facts.md](references/measured-facts.md).

## Checklists

**A new gate.** Its verdict is its exit status, captured before anything filters the output; no gate sits in the middle of a `;` chain; and the test executes the shell rather than pattern-matching it ([references/exit-status.md](references/exit-status.md)). A probe on the development Mac is not evidence about the Linux builder: run it in `ubuntu:24.04`.

## Where the tests are

- `scripts/scripts_test.go`, `scripts/kernel_gate_test.go`, `scripts/guest_image_pipefail_test.go` and the other `scripts/guest_image_*_test.go` files execute the scripts against passing and failing fixtures.
- `internal/provider/codebuild/buildspec_test.go` parses the buildspec and runs its commands under `/bin/sh`.
- `internal/provider/ec2/build_freespace_test.go`, `internal/provider/ec2/toolcache_gate_test.go`, `internal/provider/ec2/payload_test.go` (the user-data budget).
- `internal/provider/tart/realguest_test.go` measures the launcher delivery in a real guest.

## Related skills

`billet-guest-images` (what the gates protect), `billet-providers-aws` (the buildspec and provisioning script), `billet-providers-local` (the guest launcher), `billet-testing` (execute, do not pattern-match).
