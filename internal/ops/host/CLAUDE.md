# internal/ops/host

The host's commands: `billet check` and its preflights, `local up|down|status|backup|restore|recover|uninstall|prepare`, `host-upgrade`, `converge-guard`, `rollout`, `release inspect|record`, `fleet`, `server retire`, the node's `migrate-endpoint` and `receipt`, and the systemd notification both roles send. cmd/billet dispatches to it and nothing else imports it. It is the one command family allowed to import the others (`opsconsumers` in `.golangci.yml`): a host's lifecycle drains through fleetops, installs a restored App key through setup's writer and pulls Tart images through images, and the check reads every family's configuration. Load `billet-lifecycle`, `billet-releases-and-upgrades`, `billet-controller-retirement` or `billet-backup-restore` for the area a change is in.

- The order a command acts in is often its whole safety content, and structural tests (`upgradefence_test.go`, `downgrade_test.go`, `rolebudgets_test.go` and others) assert it by parsing this package's source. Move a call and its test together.
- Every ledger open is a mode of `internal/app`'s `OpenLedger`; `ledger.go` names the modes for the commands that choose an opener as a value, and `ledgerrelease_test.go` holds each name to its mode. Retirement opens its own restricted handles (admin, inspection, completion) in `serverretire*.go`; never replace one with an ordinary helper.
- `convergeguardgatecrash.go` builds only under the `billetgatecrash` tag the collection's gate uses, and `convergeguard_dev_linux.go`, `convergeguard_scan_linux.go` and `releaseinspect_linux.go` only on Linux: `make lint` and `make cross` see them, a darwin `go vet` does not.
- The retirement tests (`^TestRetirement`) need PostgreSQL and run in CI's retirement shards, which `scripts/check-retirement-shards.py` partitions from this directory; the PostgreSQL command job runs the rest.

Gates: `make check`; `make cross` before anything touching a build tag.
