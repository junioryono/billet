# ADR-023: Windows jobs run on EC2 first, from GitHub's own steps

Accepted, 2026-11-04. Issue #532. Extends [ADR-002](adr-002-cloud-compute-backend.md) to a second guest operating system, and departs from [ADR-022](adr-022-every-linux-image-from-githubs-template.md) in how much of GitHub's template one image runs.

## Context

A team whose workflows include `windows-latest` has had to keep GitHub-hosted runners for those jobs. Every backend, the guest image and every launcher assumed Linux or macOS. `config.GuestWindows` existed and a tier could say it, but nothing behind it could start a runner.

Three backends were candidates: an EC2 instance per job, a Windows guest under KVM on a Linux node, and a billet node running on Windows itself. The issue asked for all three to be measured before choosing.

## What was measured

All of this ran in `us-west-2` on 2026-11-04, against `Windows_Server-2025-English-Full-Base-2026.10.14` (`ami-018876cb673bc1709`) and, for KVM, Microsoft's Windows Server 2025 evaluation media (build 26100.1742). Nothing built from either was published, and every machine was terminated.

**KVM on a Linux node boots far faster than EC2 does.** On a `c8i.2xlarge` with nested virtualization enabled, under QEMU 8.2.2, with 4 vCPU and 8 GiB given to the guest and a virtio disk on gp3:

| | |
|---|---|
| Unattended install, ISO to first logon | 380 s |
| Cold boot of a qcow2 overlay to a startup task's first PowerShell line, three runs | 17.3 s, 17.2 s, 17.0 s |
| The installed image | 10.4 GiB |
| Each overlay after boot | 53 to 54 MiB |
| QEMU's resident memory 20 s after that line | 6.3 GiB of 8 GiB |

Nested virtualization bounds bare metal from above, so a real host should do no worse. Those boots are of a machine that has booted before and carries nothing but Windows, and the EC2 numbers below are of an image first read from its snapshot, so the two are not the same measurement; the gap is still large enough to be real.

**EC2's first boot is the cost.** Launch to the first line of a new instance's user data, on `c7i.xlarge`, two samples each:

| Image | Launch to user data |
|---|---|
| Amazon's AMI, as published | 62 s |
| Made after `ec2launch reset` | 113 s |
| Made after `ec2launch sysprep` | 247 s |

**And a page file in the image costs a job its assignment.** The first complete image reached user data at 369 s of uptime, in both samples, with almost no CPU used. The system log stands still for 5 min 6 s between the network driver loading and session start, and a warm reboot of the same volume crosses that gap in under a second, so it is blocks being read from the snapshot: Session Manager reading the `pagefile.sys` the build left behind. GitHub cancels an assignment left unstarted after five minutes ([the assignment record](../records/assignment-protocol.md)), so billet retired each runner mid-boot and launched another, and no job started.

| The same image, `c7i.xlarge` | User data, uptime | First listener start, uptime | Launch to that | Added per launch |
|---|---|---|---|---|
| As built, with the page file | 369 s, 369 s | 426 s, 431 s | 518 s, 524 s | |
| `VolumeInitializationRate` 300 MiB/s | 85 s, 87 s | 91 s, 95 s | 163 s, 154 s | $0.13 |
| `VolumeInitializationRate` 200 MiB/s | 112 s | 122 s | 180 s | $0.09 |
| No page file in the image, paging restored | 91 s, 91 s | 151 s, 151 s | 229 s, 235 s | |

The build therefore runs with paging off and restores it, with no file, as its last act, so each job's boot creates a new one; the verifier refuses an image that boots without one.

**The free fix alone is not enough.** With no page file a real job started 311 s after its launch, and GitHub had its assignment taken up 4 min 46 s after the dispatch, 14 s inside the five minutes, because every file the listener loads is still the first read of its blocks. A launch that asks for `VolumeInitializationRate` 200 started the same job 121 s after launch, 1 min 59 s after the dispatch, so every Windows launch asks for it. AWS charges it per launch on the snapshot's full size, the same from 100 to 200 MiB/s: $0.0024 per GB, about $0.09 on this 35.6 GiB image (read 2026-11-04). A missed assignment costs a second boot and five more minutes of queue, which is what that buys off. A Linux runner starts in under a minute and does not ask.

@@EC2_JOB_TIMING@@

**A billet node on Windows is a port, not a backend.** Type-checking every package for `GOOS=windows` with `go/packages`, which reports past a broken dependency where `go build` stops at the first: 21 of 95 packages fail, with 502 errors. Most are `flock`, `O_NOFOLLOW`, `O_DIRECTORY`, `Stat_t`, `openat` and `fstatat`, which is to say the state lock and the descriptor rules `internal/regularfile` and `internal/hostauthority` exist to hold (`billet-security`), and those rules have no direct equivalent: Windows has no `O_NOFOLLOW`, and a file lock there is mandatory and is not inherited. `internal/ops/host` (173) and `internal/provider/firecracker` (171) are the largest, and the second would simply not build there. `internal/lifeops` assumes systemd or launchd, so a Windows service would be a third lifecycle. And it would still need a backend: a node on Windows starts Hyper-V guests or runs jobs on itself, and the second has no boundary at all.

**Firecracker cannot boot Windows.** Its supported guests are Linux and OSv, and it offers neither the UEFI nor the devices Windows needs, so KVM here means a new backend (QEMU or Cloud Hypervisor), not an option on the existing one.

## Decision

**The first Windows backend is EC2, one instance per job, launched from an AMI `billet ami build --os windows` makes in the operator's own account from Amazon's license-included Windows Server 2025 image.**

KVM boots faster and costs nothing per hour on a host someone owns, and that is why it is not ruled out. It loses here on what it needs before the first job can run:

- **A licence per host.** Amazon's AMI includes the licence in the hourly price. On hardware someone owns, running a guest per job means Windows Server Datacenter on every core of the host, or Standard's limit of two guests. billet cannot accept that on an operator's behalf, and the evaluation media measured above is for evaluation.
- **An image nobody may hand them.** billet cannot publish a Windows disk image as it publishes the Linux guest (ADR-014, ADR-021's reasoning for macOS), so every deployment would build its own from media it supplies, on a backend that does not exist yet.
- **A new backend under every rule `billet-add-provider` names**, with its isolation, networking and cache questions, for a guest whose cost EC2's price already states.

EC2 needs none of these and runs under every rule the backend already keeps: lease, teardown, custody, the trust split and the fallback order. A Windows tier that later gains a KVM node changes `providers:` and nothing else, so nothing here forecloses it. What would change this decision is a deployment with Windows volume high enough that per-hour licensing costs more than per-core licensing, and a licence it already holds.

**A Windows image is built from GitHub's own steps, a declared subset of them.** `actions/runner-images`' `images/windows` is vendored at the commit billet already pins, and `plan-windows-2025.json` is derived from its `build.windows-2025.pkr.hcl` by the reader that derives the ubuntu plans. `scripts/windows/build-ami.ps1` runs every step `differences-windows.tsv` does not skip, as Packer's `powershell` provisioner would: a new PowerShell with the account's profile, the machine's environment as it stands now, the template's variables, and the exit status as the verdict. Every step left out has its reason on one line.

ADR-022 rejected reimplementing what GitHub installs, because every gap was then found by a user's workflow, and this does not. It rejects running the whole of that template on Windows for the first image, for reasons that are not ADR-022's:

- Visual Studio 2022 Enterprise needs a subscription this build cannot accept on anyone's behalf, and much of the template stands on it (`Install-VSExtensions`, `Install-Wix`, `Install-ServiceFabricSDK`, `Configure-User`).
- The template restarts six times, for Windows features and updates a first image does not need.
- `Install-WindowsUpdates` costs hours for a base AMI Amazon republishes patched each month.

So moving towards GitHub's image is removing a line from a list, and a workflow that needs a tool names the line. ADR-022's rule stands for every Linux image.

## What a Windows image carries against GitHub's `windows-2025`

Read from `toolset-2025.json` and the template at the pinned commit (`e75633902841`). The Pester test each carried step runs is part of the build, as it is in GitHub's.

| | On a billet image | How |
|---|---|---|
| Windows Server 2025 | Yes, Amazon's build of it | the base AMI, patched to its month |
| The Actions runner | Yes, pinned by digest | `ec2-layer.ps1` |
| PowerShell 7 at the declared line, and the declared PowerShell modules | Yes | `Install-PowershellCore`, `Install-PowerShellModules` |
| Git for Windows with Git Bash, Git LFS, GitHub CLI | Yes | `Install-Git`, `Install-GitHub-CLI` |
| Toolcache: Python, Node, Go, Ruby, PyPy, every declared line | Yes | `Install-Toolset`, `Install-Ruby`, `Install-PyPy`, `Configure-Toolset` |
| Node on PATH with its global packages | Yes | `Install-NodeJS` |
| Chocolatey's declared set (7-Zip, jq, cmake, ninja, NuGet, vswhere, Strawberry Perl, Julia and the rest) | Yes | `Install-Chocolatey`, `Install-ChocolateyPackages` |
| zstd, the action archive cache | Yes | `Install-Zstd`, `Install-ActionsCache` |
| Defender off, UAC off, long paths on, the template's service and scheduled-task settings | Yes | the template's `Configure-*` steps |
| Visual Studio, its extensions, Wix, Service Fabric, native-image warm-up | No | needs a subscription |
| Docker, Windows containers, Hyper-V, WSL, other Windows features | No | needs reboots, and containers are out of scope |
| Android SDK, CodeQL, Chrome, Edge, Firefox and their drivers | No | a deployment's to accept (#584), and `images.accept` completes Linux images only |
| Java, Kotlin, .NET SDKs, PHP, Rust, Haskell, R, Miniconda, MSYS2, MinGW, LLVM, vcpkg, Bazel, Azure and AWS CLIs, and the databases and servers | No | beyond this floor; `actions/setup-*` downloads what has a setup action |
| Windows updates past the base AMI's month | No | hours, against a monthly base |

`differences-windows.tsv` is the complete list, step by step.

**Three things differ from GitHub's image in ways a job could notice.**

- The account is `runner`, where GitHub's is `runneradmin`. It is a local administrator, for the same reason: a job can install what it needs. The instance is the boundary, and it is destroyed with the job.
- EMS stays off, which costs a job's instance its SAC, and nothing reaches that through an egress-only group.
- billet's caches have no client in a Windows guest, so a Windows tier sets no `cache`, `intercept` or `cache_scope`, and the node prepares no bearer for one.

## How the build reports

ADR-022's rule holds: billet images a stopped builder only after reading a success verdict bound to this build's nonce on its live console. Three things had to be measured to keep it on Windows, and each contradicted what the Linux path assumes.

- **COM1 does not exist on the stock AMI.** It boots with EMS on, so SAC owns the port. EC2Launch writes its console lines through a private `DeviceIoControl` on `\\.\EC2WinUtil`, which has no public contract. So the first boot switches EMS off and reboots, and only the second can report anything.
- **A line reaches `GetConsoleOutput` only while the port stays open.** `Close` discards what the driver has not sent, so the build holds one port throughout.
- **`ec2launch reset --shutdown` exits 1 for a machine that then stops cleanly.** So a tail that fails is one that leaves the machine up.

The build's output goes to logs under `C:\billet-ami\logs`, and on a failure a bounded tail of the failing step's log goes to the console ahead of the verdict, because a builder in an egress-only group has no other way to say why. billet logs those lines to the operator, and keeps them out of the error, since a guest wrote them. That tail named the first real build's failure at once: Amazon's AMI carries the modular AWS Tools, 429 `AWS.Tools.*` modules on every module path, so GitHub's `Install-Module AWSPowerShell` refuses to override their commands, which never happens on Azure's image. One `prepare` line moves them aside, and a job finds the `AWSPowerShell` module GitHub's image has.

## Consequences

- **The image is `reset`, not generalized.** A `sysprep` image takes 134 s longer to reach its user data, on every job, for a generalization a standalone machine that lives for one job does not need: every instance of it has the same machine SID, which matters to a domain and to nothing here. EC2 Fast Launch requires a sysprepped image, so taking it up later means changing this and measuring both. A workflow that joins a domain would need the other tail.
- **A registration in a scheduled task's world.** A task inherits nothing from the process that starts it, so the registration travels in a file readable by `runner`, SYSTEM and the administrators, which the service script reads and deletes before the listener starts. A job's steps are administrators and can still read the user data, as on Linux, so what protects the registration from them is that the runner has already spent it (ADR-002). It is never in an argument list, and the password the task logs on with is random per boot and passed through a cmdlet's parameter. The task runs at priority 4, normal, because a task's default of 7 is below normal and every process of every job would inherit it.
- **`billet check` and the launch both hold a tier's `guest_os` to its AMI**, in both directions, and refuse Amazon's own AMI under the command only billet's image carries. A Windows image answers to `WindowsAMIContract`, because every number of `AMIContract` is about a Linux image.
- **A node below wire 38 is not sent a Windows lease.** No wire struct changed, but such a node would write a Linux boot script for a Windows AMI, which boots, looks healthy, registers nothing and holds the lease.
- **A failed start ends the machine, and not the lease.** A boot script that cannot start the runner, or a listener that ends having run no job, stops the instance rather than leaving a licence billing for something that will register nothing. That ends the instance's charge and not its volume's. The lease stays charged until a Destroy, as it does for a Linux boot that fails, because a job GitHub still expects is capacity billet owes (`TestAStalePromiseIsReportedAndKept`). A stopped instance of either operating system reports `Ended`, so a drain does not wait on it, and it authorises nothing else. Releasing the lease of a stopped instance that never ran a job would be a change to every backend's custody rules, and it is not made here.

## What this does not cover

- **The tests' PowerShell is 7's, and these scripts run under Windows PowerShell 5.1** until the template installs 7. No Linux CI lane has 5.1, so `pwsh` catches what is broken in both, and only a real build proves the rest. A build is the test of a change to these scripts.
- **The scheduled task's own behaviour has no test off Windows.** `billet-runner.ps1` and `billet-runner-service.ps1` parse under `pwsh` and are held by digest by the verifier; the acceptance record is what shows them running.
- **Linux containers inside a Windows guest**, and Windows containers, are out of scope (#532).
- **A Windows node, and a Windows guest under KVM**, remain open. Their costs are above.
