# Installation

billet is one static binary. There is nothing else to install for a trial.

## The install script

```bash
curl -fsSL https://raw.githubusercontent.com/junioryono/billet/main/scripts/install.sh | sh
```

It downloads the latest release for your platform, verifies the published checksum, records which signed release manifest produced the binary, and puts `billet` in `/usr/local/bin`. It does not create users, write config, or start anything.

The script reads a few environment variables:

| Variable | Default | Meaning |
|---|---|---|
| `BILLET_INSTALL_DIR` | `/usr/local/bin` | where the binary goes |
| `BILLET_VERSION` | `latest` | an exact version such as `v0.5.2`; an exact version consults no channel |
| `BILLET_CHANNEL` | `stable` | which signed release channel `latest` resolves through |
| `BILLET_OS`, `BILLET_ARCH` | detected | set both to stage a binary for another machine |

Supported targets are `linux/amd64`, `linux/arm64` and `darwin/arm64`. A cross-target install verifies and places the binary without executing it, which is how an Ansible control machine on macOS prepares a Linux server:

```bash
stage=$(mktemp -d)
curl -fsSL https://raw.githubusercontent.com/junioryono/billet/main/scripts/install.sh | \
  BILLET_OS=linux BILLET_ARCH=amd64 BILLET_INSTALL_DIR="$stage" sh
```

## The Linux packages

For a Linux machine that should run jobs across reboots, install the package instead: it ships the systemd units. Pick the file for your platform from the [latest release](https://github.com/junioryono/billet/releases/latest).

```bash
sudo dpkg -i billet_*_linux_amd64.deb    # Debian, Ubuntu
sudo rpm -i  billet_*_linux_amd64.rpm    # Fedora, RHEL
```

The package installs the binary at `/usr/bin/billet`, the units, and a config template at `/etc/billet/billet.yaml` if none exists. It creates the `billet` service account and its directories, prepares the RBD kernel client the Firecracker backend needs, and **does not enable or start anything**: installing billet must not connect a machine to GitHub before its config says something true. Removing the package removes the units and binaries and deliberately keeps `/etc/billet`, `/var/lib/billet` and `/srv/jailer`, because they hold the deployment's identity, credentials and any recoverable guest state.

If a machine has the package installed, update the package rather than re-running the install script: the script writes `/usr/local/bin/billet` while the units run `/usr/bin/billet`.

## macOS

There is no package. The binary above is the whole install, and billet ships launch agents rather than daemons, because Apple's Virtualization.framework needs an unlocked login keychain and tart's image store is per user. `billet local up` installs, starts, proves and enables the agents once you have a config and an App; run those commands as the account that will run the node, never under `sudo`. A Mac also needs Homebrew `tart`, a pulled guest image, and for fork pull requests a one-time setuid grant on `softnet`. [Run jobs on a Mac](../deploying/mac-tart.md) walks all of it.

## Next

[The GitHub side](github-side.md): create the runner group and the App.
