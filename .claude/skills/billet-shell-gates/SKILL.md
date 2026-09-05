---
name: billet-shell-gates
description: "Rules for every shell script billet writes or generates and for every gate that must end a build: scripts/*.sh, the guest image and kernel builds, the EC2 provisioning script and AMI verifier, the CodeBuild buildspec, the toolcache installers, the guest launcher. Load before editing any .sh file, any Go that emits shell, or any pipeline whose exit status is a verdict; and when a build passed a check it should have failed, or a gate refused a correct artifact."
---

# Shell that has to be able to fail

## What this area is

billet's shell lives in `scripts/` (guest image, guest kernel, release, install, rehearsals), in `internal/runnerimages/install-toolcache.sh` (sourced by both the guest build and the EC2 builder), and in Go that renders shell: `internal/provider/ec2/build.go` (the provisioning script), `internal/provider/codebuild/buildspec.go`, the tart and firecracker guest launchers. Every one of them contains gates whose whole purpose is to end a build, and every rule below is a way one of those gates computed the right answer and then let the build succeed.

## Rules

**`set -e` is ignored for a pipeline that begins with `!`.** POSIX specifies it and it was measured on `/bin/sh`: the line after a failing `! printf x | grep -q .` executes and the script exits 0. Write the failing path as an `if` with an explicit `exit 1` and a message. `!` inside an `if` condition is fine, because the condition's status is consumed rather than inherited.

**A gate in the middle of a `;` chain is not a gate.** Measured under dash in `ubuntu:24.04`: in `mkdir …; curl …; sha256sum -c -; tar -xzf …` a failing checksum prints `FAILED`, execution continues into `tar`, and the compound exits 0. A trailing `test -x` does not save it, since an extracted tarball has a `run.sh`. Chain with `&&`, which short-circuits under either `set -e` regime. `set -e` is the trap, because a CodeBuild buildspec's commands run under CodeBuild's shell, not a script billet controls.

**The harness that executes generated shell must withhold any ambient setting the gate must hold without.** The buildspec test harness adds `set -e` itself (correctly, that is how CodeBuild treats a phase), so the one instrument able to catch the `;` chain had the defect masked by its own realism. A generated boot script or buildspec is executed under `/bin/sh` in a test, never pattern-matched: a script that fails to parse is an instance that starts, registers nothing and reports success.

**`grep -q` under `pipefail` returns the writer's SIGPIPE.** `printf big | grep -qxF <early match>` exits 141, a late match exits 0, no match exits 1, because `grep -q` leaves the moment it matches and `pipefail` reports the writer's status. `if ! printf … | grep -q …` therefore reads a present value as absent, intermittently. Use a here-string, which has no writer to signal. The same shape killed an EC2 build through `awk '{print; exit}'` on pypy's 106KB checksums page while identical code passed in a container.

**grep's status is three-valued.** 0 found, 1 not found, above 1 could not look. Folding an error into "not found" is the could-not-tell collapse this repository keeps removing.

**A gate's verdict is its exit status, captured before anything filters the output.** `scripts/build-guest-kernel.sh` once ended its Docker compatibility check in `| grep -Ei missing | head -30 || echo "NOTHING MISSING"`: with `CONFIG_VETH=y` removed the checker exits 1, the pipeline prints fifteen `missing` lines and then `NOTHING MISSING`, and the build exits 0. `scripts/check-guest-kernel-config.sh` captures the status first, and the checker is vendored at `scripts/kernel/check-config.sh` with its commit and sha256 in `check-config.pin`, verified before it runs and copied into the gate's private directory before it is hashed, because hashing a path and executing that path are two lookups of a name.

**Diagnostics that pipe into `head` fail a passing gate.** `grep -Fi missing "$out" | head -30` on a 2.9MB file exits 141 under `set -euo pipefail`: head leaves after thirty lines and grep takes SIGPIPE. The module refusal one branch over had `grep … | head -20` at exit 141 on a 3.2MB config, so the sentence saying why, and the deliberate `exit 1`, were never reached (the verdict stayed a refusal, which is why it survived a reading). One `awk` reads to the end and has nothing to signal.

**`grep -Ff anchor.crt bundle` matches any certificate.** It reads one pattern per line, every certificate shares `-----BEGIN CERTIFICATE-----`, and a bundle carrying a different certificate entirely matched. Execute a gate against both a passing and a failing fixture.

**`env` resolves executables, not builtins.** The privilege drop ends in `env -i PATH=… "$@"`, so what follows is resolved by env, which can only exec a file: `command -v`, `type`, `cd` and shell functions are simply not found. What made it expensive is that `/usr/bin/command` exists on macOS (an Apple stub, link count 15) and not on Ubuntu, so a gate emitted as `<drop> command -v "$1"` exited 0 here and 127 on the image, refusing every correct artifact. The working form is `sh -c 'command -v "$0" >/dev/null' <cmd>`. The rule for anything under a dropped-privilege check is not "it is on PATH" but "it is a file"; `internal/provider/ec2/toolcache_gate_test.go` asserts that structurally.

**A probe on the development Mac is not evidence about the builder.** Four mechanisms in one piece of work each reported success: a suppressed `apt-get update` made a probe report "0 lines" from a file it never fetched; an `ldd` jail copied the very library it tested for; a fake chroot prefixed only `argv[1]`; and this Mac's `grep` is ugrep, which reads UTF-16 transparently where GNU grep cannot, so a broken decoder looked correct. Run the probe in `ubuntu:24.04` or read the bytes with the builder's own tools, and prefer an assertion about bytes or emitted argv over a pipeline that happened to work somewhere.

**EC2 user data is 16384 bytes and base64 is the wrong way to spend it.** billet gzips the provisioning script (cloud-init decompresses it). Base64 of the shared installers and the pinned declaration gzips to 27235 bytes, over by ten kilobytes, because base64 costs 33% before gzip and destroys the redundancy gzip lives on; the same bytes in quoted heredocs with whole-line comments stripped land at 11327. The strip must be whole-line: `s/#.*$//` treats `${v#v}` as a comment and the result fails `bash -n`. The budget is shared, which is why `maxCACertPEM` is 8 KiB. `packUserData` is the real gate, since deliverability depends on everything else in the script.

**The toolcache installers are one file with one seam.** `install-toolcache.sh` is sourced by the guest build and carried to the EC2 builder; `billet_tc_run` is `chroot "$BILLET_TC_ROOT" "$@"` for the guest, which assembles a filesystem it is not running, and `"$@"` for the AMI, which is the target. They are bash (arrays, `declare -n`), so a `/bin/sh` caller runs a bash driver that sources the file, because a function does not cross a process boundary. Deleting the chroot is not subtle: the guest build installs five JDKs onto the build host, reports success, and ships an image with none of it, and no test that reads the script can see that.

**Every toolcache vendor spells the architecture differently, and the record has three states.** `BILLET_TC_ARCH` is required, because an unset one defaulting to x64 builds an x64 toolcache onto an arm64 image where nothing fails until a job execs a binary. `/opt/hostedtoolcache/.billet-unpublished` must be written by the narrowest question available, because "my query found nothing" is three facts: the vendor published nothing (record it), the vendor published something billet cannot verify (record it), or billet's selector went stale against a rename (fatal). Collapsing the third into the first ships an image with none of a runtime past a gate that accepts the record.

**`pgrep -f <marker>` matches its own invoking shell.** Split the literal (`billet-surv[i]vor-marker`) or every mechanism reports one survivor and the probe proves nothing. And a trailing `[ -s pid ] && printf pid` makes the whole query exit non-zero when there is no pid, which reads as "could not ask" and silently disabled a conclusive verdict in the guest launcher.

**Vendored scripts are pinned to LF.** `.gitattributes` marks `scripts/kernel/check-config.sh` `text eol=lf`, because the release gate refuses a copy that does not hash to the pin, and a line-ending conversion on checkout changes every byte.

## Measured facts

- A fresh `ubuntu:24.04` container's `apt-get update` against a mid-sync archive mirror (2026-09-05): eighteen minutes of silence inside a thirty-minute job, then killed with nothing to read; the run that did finish said `File has unexpected size ... Mirror sync in progress?` after ten. Every apt call in the rehearsal and lifecycle gates (`restore-rehearsal.sh`, `postgres-restore-rehearsal.sh`, `systemd-lifecycle.sh`, `test-package-lifecycle.sh`, `rehearsal-lib.sh`, `test-systemd-lifecycle.sh`) is now `timeout -v -k 10 300 apt-get -o APT::Update::Error-Mode=any -o Acquire::Retries=3 -o Acquire::http::Timeout=30`, and the two container entrypoint strings keep apt's stderr so `docker logs` carries the reason; the guest image and kernel builds are not gates and keep bare apt. The systemd readiness loops wait 720s, longer than the two bounded calls they follow, and stop at once on a container that has exited. Against a black-holed proxy, `apt-get update` without `Error-Mode=any` exited 0 on "Some index files failed to download"; with it, 100.
- `! cmd | grep -q .` under `set -e`: the script continued and exited 0 (`/bin/sh`).
- `;` chain with a failing `sha256sum -c`: exit 0 under dash in `ubuntu:24.04`.
- `grep -q` early match under `pipefail`: exit 141; late match 0; no match 1.
- `grep | head -30` on 2.9MB: exit 141; `grep | head -20` on 3.2MB: exit 141.
- Base64 payload gzipped: 27235 bytes; heredoc payload: 11327; limit 16384; a 32652-byte CA bundle takes the payload to 24239, hence the 8 KiB bound.
- `/usr/bin/command`: present on macOS (link count 15), absent on Ubuntu; `env -i … command -v sh` exits 0 here and 127 on the image.
- pypy ships aarch64 builds for 3.9 and 3.10 that its checksums page does not cover (115 aarch64 lines, neither release).
- moby's `check-config.sh` exits 1 for a cgroup v1 hierarchy not mounted, for apparmor without `apparmor_parser`, for `kernel.keys.root_maxkeys <= 10000`, and aborts under `set -e` where `sysctl` is missing; all facts about the build host, named in the refusal.

## Where the tests are

- `scripts/scripts_test.go`, `scripts/kernel_gate_test.go`, `scripts/guest_image_pipefail_test.go` and the other `scripts/guest_image_*_test.go` files execute the scripts against passing and failing fixtures.
- `internal/provider/codebuild/buildspec_test.go` parses the buildspec and runs its commands under `/bin/sh`.
- `internal/provider/ec2/build_freespace_test.go`, `internal/provider/ec2/toolcache_gate_test.go`, `internal/provider/ec2/payload_test.go` (the user-data budget).
- `internal/provider/tart/realguest_test.go` measures the launcher delivery in a real guest.

## Related skills

`billet-guest-images` (what the gates protect), `billet-providers-aws` (the buildspec and provisioning script), `billet-providers-local` (the guest launcher), `billet-testing` (execute, do not pattern-match).
