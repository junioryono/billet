# Your first config

`billet init` writes a `billet.yaml` that runs, with nothing to hand-edit afterwards.

```bash
billet init \
  --org your-org \
  --runner-group billet-trusted \
  --workflow 'your-org/your-repo/.github/workflows/ci.yml@refs/heads/main' \
  --config ~/billet.yaml
```

It measures the machine and writes a capacity ceiling below what it found, leaving room for the kernel, Docker and your shell. It then generates tiers that fit **together** under that ceiling, because each tier reserves one slot before it advertises, and a catalogue whose tiers cannot all be served at once would leave some of them undiscoverable. It picks a runner image that is actually pullable, points the state directories somewhere writable, and describes both roles in one file.

The App ids are `0` because the App does not exist yet; [creating the App](github-side.md) fills them in.

## What the file says

```yaml
server:
  listen: 127.0.0.1:7717      # loopback: the two processes talk over plain HTTP
  state_dir: ~/.local/state/billet/server
  max_vcpu: 6                 # the ceiling init measured, minus headroom
  max_memory: 12GiB

github:
  org: your-org
  app_id: 0                   # written by billet github-app create
  installation_id: 0
  private_key_path: ~/.local/state/billet/app-private-key.pem

node:
  server_addr: 127.0.0.1:7717
  provider: docker
  state_dir: ~/.local/state/billet/node

tiers:
  - label: billet-2vcpu       # the runs-on value
    trust: trusted
    runner_group: billet-trusted
    workflows:
      - your-org/your-repo/.github/workflows/ci.yml@refs/heads/main
    vcpu: 2
    memory: 4GiB
    image: ghcr.io/actions/actions-runner:latest
```

The shape is the whole product in miniature: a `server` block for the control plane, a `github` block for the App, a `node` block for this machine as a compute host, and `tiers`, where each tier is one `runs-on` label and one GitHub scale set. [Configuration](../reference/configuration.md) documents every key.

## Two rules about the file

**Re-running `init` converges what it owns and refuses to guess about the rest.** If the file carries content it cannot merge for you (edited values, sites, extra tiers), it writes `billet.yaml.new` beside it for a deliberate comparison instead of touching the original; `--force` overwrites outright. `billet github-app create --config` follows the opposite rule: it edits a single block into a file you already have. Each command states which rule it is following before it acts.

**`--config` is not optional.** billet does not read a `billet.yaml` from the working directory, because a server started from a directory someone else can write to would otherwise adopt their config, which chooses the state directory, the App key path and every tier's resources. Without the flag it reads your user config directory; `billet check -h` prints the path.

## Other generations

- `--profile local-service` writes the packaged paths (`/etc/billet`, `/var/lib/billet`) for a machine that will run the systemd units or launch agents.
- `--provider tart --node-name <this-mac>` writes a Mac config, and is refused anywhere but on the Mac itself, because the ceiling, the paths and the images all come from the machine running the command.
- `--provider firecracker`, `--provider ec2` and `--provider codebuild` write the block each backend needs; the [deploying](../deploying/choose-a-shape.md) guides show them.
- `--emit ansible` prints the `billet_config` block for an Ansible inventory instead of writing a file.

## Next

Back to [the GitHub side](github-side.md) to create the App, then [your first job](first-job.md).
