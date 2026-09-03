# Single host with Docker

One machine running both roles with the Docker backend. This is the [Get started](../getting-started/index.md) tutorial made persistent: the same GitHub-side steps, then the systemd units instead of two terminals.

## What it is for

Trusted workflows from private repositories, on a machine you already have. A container shares the host kernel, so billet refuses untrusted work on this backend outright; a fork pull request needs [a Firecracker host](linux-firecracker-host.md), [a Mac](mac-tart.md) or [EC2](aws-ec2.md).

## Steps

1. Install the package ([Installation](../getting-started/installation.md)). It creates the `billet` account, `/etc/billet`, `/var/lib/billet`, the units, and a config template it does not start.
2. Create the runner group and generate the config with the packaged paths:

   ```bash
   sudo -H -u billet billet init --profile local-service \
     --org your-org --runner-group billet-trusted \
     --workflow 'your-org/your-repo/.github/workflows/ci.yml@refs/heads/main'
   ```

3. Create the App. The private key must land somewhere the service user can write; `/etc/billet` is root-owned:

   ```bash
   sudo -H -u billet billet github-app create --org your-org \
     --key-path /var/lib/billet/app-private-key.pem --config /etc/billet/billet.yaml
   ```

4. Check and start:

   ```bash
   sudo -H -u billet billet check --config /etc/billet/billet.yaml
   sudo billet local up
   ```

   `billet local up` runs `billet check`, starts the server, then the node, proves each held its process, and only then enables the units. It refuses to start a control plane on an organization until the App has been verified, refuses to touch an active service, and writes no unit files itself.

5. `billet status` lists the tiers; point `runs-on` at one.

## What to know about this shape

- **`billet-node` is root on that host.** It joins the `docker` group, and anything that can reach a rootful Docker socket can start a privileged container. Prefer rootless Docker where the workload allows it.
- **The machine is the deployment.** Power, network, controller, compute and state fail together. Back it up ([Backup, restore and recover](../operating/backup-restore-recover.md)) and enable the daily timer: `systemctl enable --now billet-backup.timer`.
- **`billet local down` is a drain that then stops**: it seals admission, waits for running work with no default time limit, stops the node, stops the server, disables both. `billet local uninstall` is that plus forgetting the services, leaving the data.
- **It is not repeatable by hand.** The same deployment converged by Ansible, so the next machine and the next upgrade are one command, is the collection's [single-host-docker example](https://github.com/junioryono/billet/tree/main/ansible_collections/junioryono/billet/examples/single-host-docker).
