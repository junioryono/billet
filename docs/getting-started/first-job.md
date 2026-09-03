# Your first job

## Check

```bash
billet check --config ~/billet.yaml
```

This proves the key signs a JWT GitHub accepts, the App is installed on the organization you named, the permissions are exactly what was requested, and, for each trusted tier, that its runner group exists, is workflow-restricted to exactly your list, and grants at least one repository. It validates the config and the state directory too, and it works while a control plane is running, so it is the first thing to run whenever something looks wrong.

## Start

Two terminals:

```bash
billet server --config ~/billet.yaml   # the control plane
billet node   --config ~/billet.yaml   # the machine that runs jobs
```

The server registers one scale set per tier and long-polls GitHub; watch for `scale set ready`. The node dials the server over loopback and calls `docker ps` before it takes any work, to re-adopt containers from a previous run; watch for `registered with the control plane`. Nothing is exposed to the network and no certificates are involved, because there is nothing between two processes on one box to authenticate.

On a machine with the package installed, `billet local up` does the same as a service: it runs `billet check`, starts the server, then the node, proves each held its process, and only then enables the units. [Start and stop](../operating/draining-and-stopping.md) covers the rest of that lifecycle.

## Run something

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

- **Check `billet status` first.** If your tier shows `0 available`, billet is advertising nothing and GitHub has nothing to assign. Usually another tier's reservation is holding the memory: reduce a tier, or raise the ceiling if the machine has room.
- **Check the label matches a tier exactly.** `runs-on` matches the tier's label, which is the scale set's name.
- **Check the runner group grants the repository.** A group with selected visibility and no repositories looks exactly like a healthy deployment from every other angle.
- **Check the workflow is on the group's allowlist, including the ref.** `…/ci.yml@refs/heads/main` does not match a run on another branch.
- **Cancel old queued runs before retrying.** A backlog of runs that were never assignable can keep new dispatches queued.

[Troubleshooting](../operating/troubleshooting.md) has the longer list.

## Next

[Next steps](next-steps.md).
