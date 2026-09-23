# Run jobs on a Mac

One job, one ephemeral VM on an Apple Silicon Mac you own, through [tart](https://tart.run) and Apple's Virtualization.framework. A macOS guest runs Xcode; a native arm64 Linux guest on the same Mac does the two things a macOS guest cannot, a `docker build` and a service container. Both have run real jobs to green and been destroyed afterwards. There is no duration limit, which is what separates this from managed macOS in AWS.

## What the Mac needs that a Linux host does not

- **Apple Silicon and a recent macOS.** Virtualization.framework is what runs the guests, and a macOS guest is only legal and only possible on Apple hardware. Apple's licence permits two concurrent macOS guests per physical Mac, which is what `config.DefaultMacOSVMLimit` encodes and what the hypervisor itself refuses beyond ("The number of VMs exceeds the system limit").
- **One session with a display and a keyboard**, once. Setup Assistant has no headless path without MDM. Everything after the first login is remote.
- **Automatic login.** Since macOS 15, Virtualization.framework needs an unlocked `login.keychain`, and a headless SSH session leaves it locked; the failures (`SecKeyCreateRandomKey_ios failed`, `Interaction is not allowed with the Security Server`) name no keychain. A real GUI session at every boot keeps it unlocked, which is a genuine security decision and the price of running macOS guests with nobody present. The first GUI login creates the keychain, whether at the Mac's own display or through Screen Sharing; an SSH login alone never does.
- **FileVault off, decided in Setup Assistant.** macOS offers no automatic login while FileVault is on (Apple's documented rule, not one billet measured), so the requirement above settles the question Setup Assistant asks. It also decides what a power cut costs: with FileVault on, a Mac that comes back on waits at the unlock screen, and until someone unlocks it and starts the account's GUI session no agent starts and every job for its tiers queues (Apple documents unlocking over SSH on macOS 26, which saves the trip but is still an operator, not an unattended recovery). The price is data at rest: FileVault off removes password protection from the volume key, so with automatic login, whoever holds the Mac holds the runner account's files and credentials, the App key included when the Mac also runs the control plane; keep it physically secured. Once FileVault is off, set System Settings → Users & Groups → Automatically log in as, to the account that will run the node.
- **Homebrew `tart`**, and for untrusted work a one-time setuid grant on `softnet`.
- **Disk.** The Xcode image is about 87 GB on disk (140 GB virtual) before clones; the arm64 Linux `ubuntu-runner-arm64` image is 11.3 GB compressed on a 40 GB virtual disk. billet refuses to pull inside a launch because a node executes one command at a time; pull before the first job.
- **Not 10GbE.** A Mac never joins a Ceph site (`node.ceph` is refused on a tart node), so its traffic is GitHub polling, image pulls and artifact uploads.

The [reference Mac](../reference/reference-hardware.md) is a Mac mini with 64 GB and 1 TB, sized for two comfortable macOS guests plus a Linux tier; a 24 GB mini running one macOS guest is a real deployment. Unless a passage below attributes it otherwise, every fact about tart, its guests and softnet was measured on an M2 Max running macOS 26, tart 2.36.0 and softnet 0.23.0; what the first-boot steps rest on was measured on the reference mini under macOS 27.0 and is recorded in [Headless operation](../reference/reference-hardware.md#headless-operation).

## Before billet: Setup Assistant and the first boot

Done once, at the Mac, in this order; nothing here can be done remotely before it.

1. **In Setup Assistant**, the account name is the account that will run the node: its GUI domain holds the launch agents and its home holds tart's images, and renaming a macOS account later is its own project, so choose it now. Turn **FileVault off** (above). An Apple ID is not needed: the host needs no App Store, because Xcode lives in the macOS guest image, and the command-line tools Homebrew installs are the only developer tools the host itself uses. Set updates to download but not to install macOS updates automatically, because an install restarts the host under whatever it is running.
2. **In System Settings → General → Sharing**, turn on Remote Login, restricted to that account. In **Users & Groups**, set Automatically log in as to it. The account keeps its password; automatic login only skips the login window at boot, and SSH and `sudo` still ask for it. Leave **Screen Sharing off** on a Mac that will run untrusted tiers: it takes the account's password, and an untrusted guest reaches it at its gateway (below). `sudo launchctl disable system/com.apple.screensharing && sudo launchctl bootout system/com.apple.screensharing` over SSH turns it off (measured to close the port; whether the override holds across a reboot is not yet measured). If you need the screen later, take untrusted work off the Mac first: `billet local down`, then confirm no guest is left: `pgrep -fl 'tart run'` prints nothing, since `tart run` is the VM whatever store it came from (`tart list` alone reads one store, the one its `TART_HOME` names). `down` proves the node exited, not that every guest did, because a guest outlives a node killed at the end of its stop grace. If you cannot establish that, leave Screen Sharing off. Only then `sudo launchctl enable system/com.apple.screensharing && sudo launchctl kickstart -k system/com.apple.screensharing` (not yet measured; keep a display and keyboard as the fallback), and turn it off and check that 5900 refuses before `billet local up`.
3. **In Terminal**, keep the machine awake and give it its node name, since `--node-name` must match `^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$` and a stock name does not:

   ```bash
   sudo pmset -a sleep 0 disksleep 0
   sudo scutil --set HostName mac-mini-1
   sudo scutil --set LocalHostName mac-mini-1
   sudo scutil --set ComputerName mac-mini-1
   fdesetup status          # FileVault is Off.
   ```

   `LocalHostName` is also the Bonjour name, so the Mac answers as `mac-mini-1.local` on the LAN whatever address DHCP gives it; a DHCP reservation is still worth making, because whatever routes to the Mac from outside names an address.
4. **Install Homebrew and tart** (`brew install openai/tools/tart openai/tools/softnet`). tart's repository moved from `cirruslabs/tart` to `openai/tart`; the old `cirruslabs/cli` tap stopped at tart 2.32.1, and Homebrew 7.0.6 refuses its formula. Rosetta is not needed. billet itself is installed in the next section, once `/usr/local/bin` exists: Homebrew on Apple Silicon lives under `/opt/homebrew`, and the install script refuses a directory that is not there.
5. **Prove the unattended boot before the Mac goes headless**, while a keyboard is still attached: `sudo reboot`, then from another machine over SSH, `who` lists the account on `console` and `launchctl print gui/$(id -u)` reports `type = login`. Only then disconnect the display. The reference mini, shut down to be moved with `autorestartatconnect` at its default of 0, stayed off when it was plugged back in until its power button was pressed; set that before a move, or expect to press the button once. From SSH, `security show-keychain-info ~/Library/Keychains/login.keychain-db` answers *User interaction is not allowed* even while the GUI session holds the keychain unlocked, because an SSH session is a different security session; that answer is not a failure, and the proof that the keychain serves Virtualization.framework is the first guest billet starts.

A macOS update is then `billet local down`, the update and its restart, and `billet local up`. `down` stops and disables the node agent, and the server agent when this Mac runs one; the scheduled upgrade and image agents stay enabled. What it waits for depends on the shape. On a node-only Mac there is no ledger to seal and `down` does not touch control-plane admission: the node stops taking work on SIGTERM and drains what it holds, bounded by the agent's stop grace (88200 seconds; see [What the agents are](#what-the-agents-are)). `billet drain` against the control plane is the way to stop admission first, but it pauses the whole deployment, and `local up` on this Mac does not undo it: finish with `billet resume` against the control plane. On a Mac that also runs the control plane, `down` seals admission for the whole deployment and waits, with no limit by default, until the deployment holds nothing, remote hosts included. Treat a major macOS version as a change to test rather than an update to take, because Virtualization.framework and tart move with it.

## Set it up, on the Mac

```bash
CFG=/usr/local/etc/billet/billet.yaml

sudo mkdir -p /usr/local/bin /usr/local/etc/billet /usr/local/var/log/billet \
              /usr/local/var/lib/billet /usr/local/var/run/billet/locks
sudo chown "$(id -un)" /usr/local/bin /usr/local/etc/billet /usr/local/var/log/billet \
              /usr/local/var/lib/billet /usr/local/var/run/billet/locks
curl -fsSL https://raw.githubusercontent.com/junioryono/billet/main/scripts/install.sh | sh

billet init --profile local-service --provider tart --node-name mac-mini-1
billet github-app create --org <your-org> --config "$CFG"
billet images pull --config "$CFG"    # the macOS image; do it before the first job
billet check --config "$CFG"
billet local up                       # installs, starts and proves both agents
billet local status
```

Run these as the account that will run the node, **never under `sudo`**: a launch agent lives in your GUI domain, and root's domain has neither an unlocked keychain nor the images you pulled. The directories are the one thing that needs `sudo`, because `/usr/local` is root-owned and launchd creates nothing itself; `billet local up` refuses with the exact command rather than asking for a password.

`/usr/local/bin` is in that list because the updater runs as this account too. A rollout upgrades a Mac by replacing `/usr/local/bin/billet`, which is a rename into that directory, and on a stock Mac the directory is root's; `billet local up` and the updater both refuse with the `chown` above rather than draining the node and then finding the replacement cannot land. On a dedicated runner Mac the account already controls every guest and every image, so the binary being writable by it adds exposure only through that account.

`billet init --provider tart` is refused anywhere but on the Mac itself: the ceiling is measured there, the paths are that platform's, and the images named are the two billet has run real jobs in. `--node-name` is the one input the machine cannot supply, because a macOS tier pins the host Apple counts its limit against and a stock Mac's hostname (spaces, an apostrophe, `.local`) is not a legal node name; billet refuses rather than inventing one. The generation writes macOS guests by default; `--guest-os linux` gives an arm64 Linux tier and passing both gives one of each. There is no Ansible path to a Mac; `billet local up` is the converge.

## Trusted or untrusted

An untrusted generation (the default) writes `node.tart.untrusted_isolation: softnet`, and `billet check` **fails** until the grant is in place, because a node that offers to confine a fork's job on a host that cannot confine it is worse than one that refuses. tart's default NAT reaches the host and lets a guest spoof the bridge; softnet is the mechanism billet drives. **softnet alone does not close the host itself**, so billet also passes `--net-softnet-block=@host`. softnet's documented default lets a guest send to its vmnet gateway, and that gateway is the Mac; `@host` is softnet's own name for it. Measured on the reference mini (macOS 27.0, tart 2.37.0, softnet 0.23.0): with softnet alone, an untrusted macOS guest and an untrusted arm64 Linux guest timed out on every private LAN and Zero Trust address tested but connected to the gateway's SSH (22) and Screen Sharing (5900); with `@host` blocked, a guest timed out on both, still reached the internet, and still renewed its DHCP lease, because softnet forwards DHCP to `bootpd` ahead of its block rules. `@host` needs softnet 0.19 or later, and `billet check` fails an untrusted node whose softnet does not accept it. Keep SSH key-only (the drop-in in [Headless operation](../reference/reference-hardware.md#headless-operation)) and Screen Sharing off anyway: a release before this one does not block the host.

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

Enable SSH first (System Settings → General → Sharing → Remote Login, or `sudo systemsetup -setremotelogin on`); on macOS 26, Screen Sharing after a reboot needs SSH already reachable, so it is what makes everything else recoverable. Harden with a drop-in that sorts ahead of Apple's own `/etc/ssh/sshd_config.d/100-macos.conf`, because the first value wins, and turn off `KbdInteractiveAuthentication` as well as `PasswordAuthentication`: keyboard-interactive is a second method that can take a password through PAM ([the drop-in](../reference/reference-hardware.md#headless-operation)). Do not reach for `pmset autorestart`: it is a silent no-op on Apple Silicon. What decides whether the Mac comes back is *Start up when power is connected* in System Settings → Energy (*Never*, *After power failure* or *Always*; Apple documents it for 2024-and-later minis under macOS 26.5 and later). Choose **Always**, which also starts a Mac that was shut down and then plugged back in (Apple advises leaving it unplugged for about 30 seconds first); from a shell, `sudo pmset -a autorestartatconnect 1` is what was set on the reference mini. Neither its effect nor recovery from a real power cut is measured on the reference mini yet.

## What a restart does

Kill the node mid-job and the guest keeps running; the next node reports it adopted the guest, leaves the runner alone, and the job finishes green on the same VM. SIGTERM is a drain: the node stops taking work, waits for the jobs already running, destroys their VMs itself and exits. A billet upgrade never kills a running job, because `tart run` is the VM and billet starts it detached.

## What an update does

A Mac updates itself the way a Linux host does, with launchd's vocabulary. `billet local up` installs two oneshot agents beside the services: `sh.billet.upgrade`, which every five minutes runs `billet host-upgrade --from-rollout` and acts on the rollout a ledger on this Mac records, and `sh.billet.images`, which daily pulls any configured tart image that is absent from the store. A node-only Mac in a larger deployment is upgraded by the coordinator's dispatch instead: its node agent starts the updater detached, in its own session, and a detached child provably outlives its agent's bootout, which was measured here rather than assumed from the guest case.

The transaction is the one [Upgrading billet](../operating/upgrades.md) describes, with a bootout where Linux has a stop (the node drains for as long as its jobs take, and every pid launchd named is proved gone), a bootstrap where Linux has a start (the same pid proved to survive a settle window, which is all launchd can prove), the two plists preserved and restored beside the config, and the binary at `/usr/local/bin/billet`. The ledger steps run only when this Mac holds a control plane. `billet local status` reports both scheduled agents; `billet local uninstall` boots them out and removes them with the rest.

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
