#!/usr/bin/env bash
# Leave nothing behind, whatever happened.
#
# THE KEY FILES FIRST AND UNCONDITIONALLY: the CI key and the App key are this
# run's and go whether or not anything WARP-related worked. The registration is
# deleted only when THIS run created it (the marker reach-cloudflare-warp.sh
# writes): with reach none nothing WARP-related is touched, so a deploy runner's
# or a laptop's own registration survives a job that did not make it.
set -u

rm -f "$RUNNER_TEMP/billet-ssh-key" "$RUNNER_TEMP/billet-app-key.pem"

if [[ -f "$RUNNER_TEMP/billet-warp-registered" ]]; then
  warp-cli --accept-tos disconnect || true
  warp-cli --accept-tos registration delete || true
  sudo rm -f /var/lib/cloudflare-warp/mdm.xml || true
  rm -f "$RUNNER_TEMP/billet-warp-registered"
fi
