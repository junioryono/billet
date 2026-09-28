# GitHub's runner-images build, as billet runs it

The guest image is built by running GitHub's own runner-images template (#250): the scripts that build the image GitHub's hosted jobs run on, not only the declaration they read. This directory holds everything that build reads.

- `upstream/` is GitHub's tree at the commit in `COMMIT`, byte for byte: the 24.04 template, the build scripts, their helpers, the tests and software-report code the template runs. `BLOBS` lists each file's git blob id and mode at that commit, and `scripts/runner_images_plan_test.go` holds the tree to it. `COMMIT` must equal the toolset pin in `internal/runnerimages/pinned.txt`, so the scripts and the declaration they read are never from two commits; the toolset itself is read from `internal/runnerimages/toolset-2404.json`, not vendored twice.
- `plan.json` is what the template does, in its order, derived from the template by the test's reader. A pin bump regenerates it (`go test ./scripts -run TheRunnerImagesPlanIsTheTemplates -update-plan`) and the diff is the review.
- `differences.tsv` is everything billet does differently, each with its reason: a `skip`, or a `prepare:<name>` the runner performs before a step. It is the whole difference from GitHub's image, stated in one place.
- `base.pin` is the Ubuntu 24.04 cloud root filesystem the build starts from: the release directory, the file, and its sha256 from that release's `SHA256SUMS`. The pinned `SHA256SUMS` was verified against Canonical's cloud-image signing key (`D2EB44626FDDC30B513D5BB71A5D6C4C7DB87C81`, "UEC Image Automatic Signing Key") on 2026-09-27; a bump verifies the new one the same way before recording its sum.

`scripts/run-runner-images.sh` executes the plan against a booted target through a driver (run as root, copy in, copy out, reboot).
