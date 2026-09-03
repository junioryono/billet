# The GitHub side

Two things happen in GitHub before billet can run a job: you create a runner group that restricts who may use these runners, and billet creates a GitHub App for itself. Do the runner group first, because `billet init` refuses to write a Docker config without one.

## 1. Create a workflow-restricted runner group

A Docker container shares the host kernel, so billet will only run *trusted* work on it, and trusted means a pool GitHub itself restricts, not a promise billet makes. In your organization: **Settings → Actions → Runner groups → New runner group**.

- Name it something you will recognise. This tutorial uses `billet-trusted`.
- Set **Repository access** to the repositories you trust, not "All repositories".
- Tick **Restrict this runner group to selected workflows** and add the workflow you will run, in full: `your-org/your-repo/.github/workflows/ci.yml@refs/heads/main`.

Both restrictions matter. billet refuses a trusted tier whose group is the default group, is not workflow-restricted, or allows workflows the config does not list, and it re-checks that against GitHub before every registration it mints.

```{admonition} The one that costs an hour
:class: warning
If you set repository access to selected repositories, make sure at least one repository is actually granted. A group with selected visibility and an empty list routes nothing and reports nothing: the scale set registers, capacity is advertised, and your job queues forever with no runner group attached. `billet check` refuses this, but it is easy to reach by editing the group over the REST API, where a `PATCH` that sets visibility without re-sending the repository ids clears the list.
```

## 2. Generate the config

This is the next page, but it comes before the App because the App creation writes into the config file. See [Your first config](first-config.md), then come back here.

## 3. Create the App

```bash
billet github-app create --org your-org --config ~/billet.yaml
```

This opens a browser, or prints a URL with `--no-browser`. GitHub asks you to re-authenticate first, because App creation is a sudo-mode action. On a remote machine, pass `--port 8765` to match your `ssh -L` forward.

The command says first what it will do to your file. It is the one command that **edits** a `billet.yaml` in place: it sets the App identity under `github:` and leaves every other value, your comments, the file's mode and its owner exactly as they were. If the config cannot take the block, or does not exist, it refuses before it reaches GitHub, because the App's private key is issued exactly once and a config that cannot record it is not something to find out afterwards.

billet requests exactly two permissions:

| Permission | Level |
|---|---|
| Metadata | read |
| Organization self-hosted runners | read and write |

There is no Contents permission. billet cannot read your code.

Two things happen and the command says so: the App is created, and then it is *installed* on the organization. Creating an App does not install it, and an uninstalled App is why `installation_id` would stay zero.

When it finishes, the App ids are in your config and the private key is saved beside it. **That key is issued once.** GitHub cannot reissue it, and billet will not overwrite it: re-running the command stops rather than minting a second App over the first one's key. Back it up with the rest of the deployment ([Backup and restore](../operating/backup-restore-recover.md)).

## Next

[Your first job](first-job.md): check, start, and run a workflow.
