# deploy

The packaged systemd units, launchd plists, the packaged `billet.yaml` template and the package scripts; `units.go` exposes the names as constants. Load `billet-lifecycle` before changing anything here, and `billet-security` for the units' hardening.

- Every line of a unit was found by installing a package; do not tidy one away.
- The package enables and starts nothing, except `billet-upgrade.timer` and `billet-images-refresh.timer`.
- needrestart must never restart a billet service, and the package seeds the exclusion.
- The Ansible role renders its own copies of the units: change both, and `scripts/gates/scenario.sh unit-parity` compares them.

Gates (CI runs them on every push; locally, `scripts/gates/verify.sh`): `scripts/gates/check.sh`. A unit or package-script change runs against real systemd in `scripts/gates/dist.sh && scripts/test-systemd-lifecycle.sh`; a plist change runs against real launchd only on a Mac, in `internal/lifeops/launchd`'s `reallaunchd_test.go`.
