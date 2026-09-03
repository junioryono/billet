# Run jobs on a Mac

One job, one ephemeral VM on an Apple Silicon Mac you own, through [tart](https://tart.run) and Apple's Virtualization.framework. A macOS guest runs Xcode; a native arm64 Linux guest on the same Mac does the two things a macOS guest cannot, a `docker build` and a service container. Both have run real jobs to green and been destroyed afterwards. There is no duration limit, which is what separates this from managed macOS in AWS.

## What the Mac needs that a Linux host does not

- **Apple Silicon and a recent macOS.** Virtualization.framework is what runs the guests, and a macOS guest is only legal and only possible on Apple hardware. Apple's licence permits two concurrent macOS guests per physical Mac, which is what `config.DefaultMacOSVMLimit` encodes and what the hypervisor itself refuses beyond ("The number of VMs exceeds the system limit").
- **One session with a display and a keyboard**, once. Setup Assistant has no headless path without MDM. Everything after the first login is remote.
- **Automatic login.** Since macOS 15, Virtualization.framework needs an unlocked `login.keychain`, and a headless SSH session leaves it locked; the failures (`SecKeyCreateRandomKey_ios failed`, `Interaction is not allowed with the Security Server`) name no keychain. A real GUI session at every boot keeps it unlocked, which is a genuine security decision and the price of running macOS guests with nobody present. First login must happen through Screen Sharing at least once, because that creates the keychain.
- **Homebrew `tart`**, and for untrusted work a one-time setuid grant on `softnet`.
- **Disk.** The Xcode image is about 87 GB on disk (140 GB virtual) before clones; the arm64 Linux `ubuntu-runner-arm64` image is 11.3 GB compressed on a 40 GB virtual disk. billet refuses to pull inside a launch because a node executes one command at a time; pull before the first job.
- **Not 10GbE.** A Mac never joins a Ceph site (`node.ceph` is refused on a tart node), so its traffic is GitHub polling, image pulls and artifact uploads.

The [reference Mac](../reference/reference-hardware.md) is a Mac mini with 64 GB and 1 TB, sized for two comfortable macOS guests plus a Linux tier; a 24 GB mini running one macOS guest is a real deployment. Every fact below was measured on an M2 Max running macOS 26, tart 2.36.0 and softnet 0.23.0.

## Set it up, on the Mac

```bash
CFG=/usr/local/etc/billet/billet.yaml

sudo mkdir -p /usr/local/etc/billet /usr/local/var/log/billet \
              /usr/local/var/lib/billet /usr/local/var/run/billet/locks
sudo chown "$(id -un)" /usr/local/etc/billet /usr/local/var/log/billet \
              /usr/local/var/lib/billet /usr/local/var/run/billet/locks

billet init --profile local-service --provider tart --node-name mac-mini-1
billet github-app create --org <your-org> --config "$CFG"
billet images pull --config "$CFG"    # the macOS image; do it before the first job
billet check --config "$CFG"
billet local up                       # installs, starts and proves both agents
billet local status
```

Run these as the account that will run the node, **never under `sudo`**: a launch agent lives in your GUI domain, and root's domain has neither an unlocked keychain nor the images you pulled. The directories are the one thing that needs `sudo`, because `/usr/local` is root-owned and launchd creates nothing itself; `billet local up` refuses with the exact command rather than asking for a password.

`billet init --provider tart` is refused anywhere but on the Mac itself: the ceiling is measured there, the paths are that platform's, and the images named are the two billet has run real jobs in. `--node-name` is the one input the machine cannot supply, because a macOS tier pins the host Apple counts its limit against and a stock Mac's hostname (spaces, an apostrophe, `.local`) is not a legal node name; billet refuses rather than inventing one. The generation writes macOS guests by default; `--guest-os linux` gives an arm64 Linux tier and passing both gives one of each. There is no Ansible path to a Mac; `billet local up` is the converge.

## Trusted or untrusted

An untrusted generation (the default) writes `node.tart.untrusted_isolation: softnet`, and `billet check` **fails** until the grant is in place, because a node that offers to confine a fork's job on a host that cannot confine it is worse than one that refuses. tart's default NAT reaches the host and lets a guest spoof the bridge; softnet is the mechanism billet drives. Pass `--runner-group` and `--workflow` for a trusted pool instead, and the generation omits the isolation rather than promising one nothing uses.

The grant, exactly as `billet check` prints it:

```bash
sudo chown root <resolved softnet path> && sudo chmod u+s <resolved softnet path>
```

Both commands in that order, always: `chown` clears the setuid bit (measured), and a setuid bit on a binary owned by the installing user grants nothing. The path is the Cellar target behind Homebrew's symlink, which changes on every `brew upgrade`, so the grant survives no upgrade and `billet check` reports it on every run. If the `softnet` on `PATH` is not beside tart's binary, `billet check` warns before printing the command.

softnet blocks the private address space, which includes the guest's DHCP resolver, so egress keeps working while name resolution dies and every job fails to clone. billet configures a public resolver in the guest before delivering the registration and proves resolution before the job starts; `node.tart.untrusted_dns` chooses the resolver.

## What the agents are

`billet local up` writes `sh.billet.node.plist` and `sh.billet.server.plist` as launch agents, clears any disabled override, bootstraps them and proves each held its process. Two settings inside them were measured: a launch agent does not inherit your shell's `PATH` (launchd's default has no Homebrew prefix, so an agent without it registers and then refuses all work with `exec: "tart": executable file not found`), and launchd's default `ExitTimeOut` is five seconds, not the twenty the man page says, so the plists set 88200 to match the Linux unit, because the node answers SIGTERM by draining for as long as its jobs run. Never set it to zero; launchd reads zero as infinity.

`billet local status` reports what launchd actually has loaded, which is not the plist: launchd reads the file once at bootstrap, so `up` compares the loaded job's program, arguments, timeout and whole environment and names `billet local down` as the way through a drift. `billet local down` drains and stops; `billet local uninstall` removes the agents and leaves your config, App key, ledger, identity and CA where they are. [Draining and stopping](../operating/draining-and-stopping.md) has the details.

## Headless operation

Enable SSH first (`sudo systemsetup -setremotelogin on`); on macOS 26, Screen Sharing after a reboot needs SSH already reachable, so it is what makes everything else recoverable. Harden with a drop-in that sorts ahead of Apple's own `/etc/ssh/sshd_config.d/100-macos.conf`, because the first value wins. Do not reach for `pmset autorestart`: it is a silent no-op on Apple Silicon, and unnecessary, because Apple Silicon minis power on when mains power returns.

## What a restart does

Kill the node mid-job and the guest keeps running; the next node reports it adopted the guest, leaves the runner alone, and the job finishes green on the same VM. SIGTERM is a drain: the node stops taking work, waits for the jobs already running, destroys their VMs itself and exits. A billet upgrade never kills a running job, because `tart run` is the VM and billet starts it detached.

## The config

```yaml
node:
  name: mac-mini-1
  provider: tart
  state_dir: /usr/local/var/lib/billet/node
  tart:
    untrusted_isolation: softnet

nodes:
  - name: mac-mini-1
    provider: tart
    guest_os: [macos, linux]
    macos_vm_limit: 2               # Apple's default; raising it is a statement about your licence

tiers:
  - label: billet-macos-6vcpu
    provider: tart
    guest_os: macos
    node: mac-mini-1                # a macOS tier pins its host
    vcpu: 6
    memory: 24GiB                   # never below 4GiB; the hypervisor refuses
    image: ghcr.io/cirruslabs/macos-tahoe-xcode:latest
  - label: billet-linux-arm64-4vcpu
    provider: tart
    guest_os: linux
    node: mac-mini-1
    vcpu: 4
    memory: 8GiB
    image: ghcr.io/cirruslabs/ubuntu-runner-arm64:latest
```

A moving image tag is resolved to its pulled digest before every clone, so what a job ran is always answerable. The published `ghcr.io/cirruslabs/ubuntu` base image carries neither the runner nor Docker; `ubuntu-runner-arm64` carries both. What is still open on a Mac: billet-built guest images with `@verified` promotion, and a cache (`node.cache` is refused there today).
