# Measured facts

Part of the `billet-providers-local` skill: what was measured, dated where it was recorded.

- Virtualization.framework refuses a third macOS guest and one under 4GiB; `config.DefaultMacOSVMLimit = 2` is pinned to that refusal.
- `tart get`: exit 2 `does not exist`; exit 1 `missing files for a supported layout`.
- `SendCtrlAltDel`: ignored by a real guest for twenty seconds.
- SIGTERM to a jailed VMM (a pid-namespace init with no handler): ignored for over two minutes; SIGKILL: immediate (2026-09-30).
- Delivery mechanisms in a real `ghcr.io/cirruslabs/ubuntu` guest: `nohup`, double fork, `systemd-run --user` all dead in three seconds; `setsid` survives.
- A node's serial queue: 300ms deadline, 60-second wait without `WaitDelay`.
- Docker `--filter name=` is a substring match.
- A container's own network counters are readable from the host without entering its namespace: `/proc/<container init pid>/net/dev` is the table of the container's namespace, readable as root and as uid 1000 from the host's pid namespace, and its `eth0` line counts from inside (rx is what the container received: 2098 bytes received against 651 sent after a download in it). A second network appears as `eth1`; the kernel's tunnel devices (`tunl0`, `gretap0`, ...) are listed in every namespace, and a name of seven or more characters has no space before its colon (Docker 29.4.3, kernel 6.12.76-linuxkit, 2026-10-08).
