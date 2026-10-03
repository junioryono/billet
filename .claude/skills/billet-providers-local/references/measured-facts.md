# Measured facts

Part of the `billet-providers-local` skill: what was measured, dated where it was recorded.

- Virtualization.framework refuses a third macOS guest and one under 4GiB; `config.DefaultMacOSVMLimit = 2` is pinned to that refusal.
- `tart get`: exit 2 `does not exist`; exit 1 `missing files for a supported layout`.
- `SendCtrlAltDel`: ignored by a real guest for twenty seconds.
- SIGTERM to a jailed VMM (a pid-namespace init with no handler): ignored for over two minutes; SIGKILL: immediate (2026-09-30).
- Delivery mechanisms in a real `ghcr.io/cirruslabs/ubuntu` guest: `nohup`, double fork, `systemd-run --user` all dead in three seconds; `setsid` survives.
- A node's serial queue: 300ms deadline, 60-second wait without `WaitDelay`.
- Docker `--filter name=` is a substring match.
