#!/usr/bin/env bash
# Leave nothing behind, whatever happened, and say so when something stayed.
#
# THE KEY FILES FIRST AND UNCONDITIONALLY: the CI key and the App key are this
# run's and go whether or not anything WARP-related worked. The registration is
# deleted only when THIS run created it (the marker reach-cloudflare-warp.sh
# writes after proving the runner had none): with reach none nothing
# WARP-related is touched, so a deploy runner's or a laptop's own registration
# survives a job that did not make it.
#
# EVERY STEP IS ATTEMPTED AND EVERY FAILURE IS REPORTED, and the marker stays
# until the registration is gone: a cleanup that swallowed a failed
# `registration delete` and removed its own record left a registration nobody
# would find again, on a runner that reported a clean job.
set -u

failed=0

rm -f "$RUNNER_TEMP/billet-ssh-key" "$RUNNER_TEMP/billet-app-key.pem"
for f in "$RUNNER_TEMP/billet-ssh-key" "$RUNNER_TEMP/billet-app-key.pem"; do
  if [[ -e $f ]]; then
    echo "::error::$f could not be removed"
    failed=1
  fi
done

if [[ -f "$RUNNER_TEMP/billet-warp-registered" ]]; then
  registration_gone=1
  timeout 60 warp-cli --accept-tos disconnect || echo "::warning::warp-cli disconnect failed; continuing with the registration"
  if ! timeout 60 warp-cli --accept-tos registration delete; then
    echo "::error::warp-cli registration delete failed; the registration this run made may still exist on this runner. The marker $RUNNER_TEMP/billet-warp-registered is kept so a later cleanup knows."
    registration_gone=0
    failed=1
  fi
  if ! sudo rm -f /var/lib/cloudflare-warp/mdm.xml; then
    echo "::error::/var/lib/cloudflare-warp/mdm.xml could not be removed; the service token stays on this runner"
    failed=1
  fi
  if [[ $registration_gone == 1 ]]; then
    rm -f "$RUNNER_TEMP/billet-warp-registered"
  fi
fi

exit "$failed"
