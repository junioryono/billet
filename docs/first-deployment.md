# Your first deployment

A single Linux machine running its own GitHub Actions runners, from nothing to a workflow completing on your own hardware. About fifteen minutes, and roughly a third of that is in a browser.

This was walked end to end on a throwaway Ubuntu 24.04 host against a real organization, and every step below is one that actually ran. Where something is easy to get wrong, it says so.

Docker is the backend here because its only prerequisite is Docker. Firecracker gives you a real kernel boundary and needs `/dev/kvm`, a guest kernel you build, a Ceph cluster, and two bridges before a job can run — that is the [deployment guide](deployment-guide.md), not a first deployment.

## What you need before you start

**A GitHub organization.** billet cannot onboard a personal account: the App is created against `/organizations/<org>/settings/apps/new`, and the runners it manages are organization runners. You need to be an owner, or someone who is has to do the two browser steps.

**A Linux machine with Docker**, which you can reach a shell on. It can be a laptop. `billet check` deliberately never touches Docker, so if the daemon is missing or your user cannot reach the socket you will not find out until `billet node` starts — check `docker ps` yourself first.

**A browser that can reach the machine's loopback.** App registration completes through a callback on `127.0.0.1`. If the machine is remote, forward it:

```bash
ssh -L 8765:127.0.0.1:8765 <the-host>
```

and pass `--port 8765` in step 3. This is not avoidable by publishing a port: the callback binds loopback only, so the forward has to terminate on the host's own loopback.

## 1. Install billet

```bash
curl -fsSL https://raw.githubusercontent.com/junioryono/billet/main/scripts/install.sh | sh
```

It verifies the published checksum before installing.

## 2. Create the runner group, in GitHub

Do this **before** `billet init`, because init refuses to write a Docker config without one.

A Docker container shares the host kernel, so billet will only run *trusted* work on it — and trusted means a pool GitHub itself restricts, not a promise billet makes. In your organization: **Settings → Actions → Runner groups → New runner group**.

- Name it something you will recognise — this walkthrough uses `billet-trusted`.
- Set **Repository access** to the repositories you trust. Not "All repositories".
- Tick **Restrict this runner group to selected workflows**, and add the workflow you will run, in full:
  `your-org/your-repo/.github/workflows/ci.yml@refs/heads/main`

Both restrictions matter. billet refuses a trusted tier whose group is the default group, is not workflow-restricted, or allows workflows the config does not list — and it re-checks that against GitHub before every registration it mints.

> **The one that costs an hour.** If you set repository access to selected repositories, make sure at least one repository is actually granted. A group with selected visibility and an empty list routes nothing and reports nothing: the scale set registers, capacity is advertised, and your job queues forever with no runner group attached. `billet check` now refuses this, but it is worth knowing why. It is easy to reach by editing the group over the REST API, where a `PATCH` that sets visibility without re-sending the repository ids clears the list.

## 3. Generate a config

```bash
billet init \
  --org your-org \
  --runner-group billet-trusted \
  --workflow 'your-org/your-repo/.github/workflows/ci.yml@refs/heads/main' \
  --config ~/billet.yaml
```

It measures the machine and writes a ceiling below what it found, leaving room for the kernel, Docker and your shell. It then generates tiers that fit **together** under that ceiling — each tier reserves one slot before it advertises, so a catalogue whose tiers cannot all be served at once would leave some of them undiscoverable.

Nothing in the file has to be hand-edited. The App ids are `0` because the App does not exist yet; the next step fills them in.

## 4. Create the App and install it

```bash
billet github-app create --org your-org --config ~/billet.yaml
```

This opens a browser (or prints a URL with `--no-browser`). GitHub will ask you to re-authenticate first — App creation is a sudo-mode action.

It says first what it will do to your file. This is the one command that **edits** a billet.yaml in place: it sets the App identity under `github:` and leaves every other value, your comments, the file's mode and its owner exactly as they are. `billet init` is the opposite — it generates a whole config, so it writes a `billet.yaml.new` beside an existing one rather than replacing it. And if the config cannot take the block, or does not exist, this refuses before it reaches GitHub rather than after, because the App's private key is issued exactly once and a config that cannot record it is not something to find out afterwards.

billet requests exactly two permissions: **metadata: read** and **organization self-hosted runners: read and write**. There is no Contents permission; billet cannot read your code.

Two things happen, and the command says so: the App is created, and then it is *installed* on the organization. Creating an App does not install it, and an uninstalled App is why `installation_id` would stay zero.

When it finishes it has written the App ids into your config and saved the private key beside it. **That key is issued once.** GitHub cannot reissue it, and billet will not overwrite it — if you re-run this command it stops rather than minting a second App over the first one's key.

## 5. Check, then start

```bash
billet check --config ~/billet.yaml
```

This proves the key signs a JWT GitHub accepts, the App is installed on the org you named, the permissions are exactly what was requested, and — for each trusted tier — that its runner group exists, is workflow-restricted to exactly your list, and grants at least one repository.

Then, in two terminals:

```bash
billet server --config ~/billet.yaml   # the control plane
billet node   --config ~/billet.yaml   # the machine that runs jobs
```

The server registers one scale set per tier and long-polls GitHub. The node dials the server over loopback — nothing is exposed to the network, and no certificates are involved, because there is nothing between two processes on one box to authenticate.

Watch for `scale set ready` in the server and `registered with the control plane` in the node.

## 6. Run something

Point a workflow at the tier's label. `billet status` lists them; on a modest machine you will have `billet-2vcpu`.

```yaml
jobs:
  build:
    runs-on: billet-2vcpu
    steps:
      - run: uname -a
```

Commit it on the branch your runner group allows, and dispatch. Within a minute the node logs `launched container` and then `destroyed container`, and the job goes green.

## When a job just sits in the queue

The failure mode of this system is a job that queues rather than an error, so this is the section you will actually use.

**Check `billet status` first.** If your tier shows `0 available`, billet is advertising nothing and GitHub has nothing to assign. Usually another tier's reservation is holding the memory — reduce a tier, or raise the ceiling if the machine has room.

**Check the label matches a tier exactly.** `runs-on` matches the tier's label, which is the scale set's name.

**Check the runner group grants the repository.** See the warning in step 2. This one looks exactly like a healthy deployment from every other angle.

**Check the workflow is on the group's allowlist**, including the ref. `…/ci.yml@refs/heads/main` does not match a run on another branch.

**Cancel old queued runs before retrying.** A backlog of runs that were never assignable can keep new dispatches queued; cancel them, then dispatch fresh.

## What this is not

A single machine running both roles is the simplest shape and the right place to start. It is not highly available — the machine is the deployment — and jobs run in containers that share the host kernel, which is why the trust restrictions above are not optional.

It is also not repeatable — you did it by hand. The same deployment converged by Ansible, so the next machine and the next upgrade are one command, is [examples/single-host-docker](../ansible_collections/junioryono/billet/examples/single-host-docker/README.md). The GitHub-side steps above are identical there; only the install and the config placement change.

When you outgrow it: [the deployment guide](deployment-guide.md) covers a Firecracker host with real kernel isolation, a control plane on its own machine with nodes dialling in over mTLS, and EC2 fallback capacity.
