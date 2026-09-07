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
# EVERY STEP IS ATTEMPTED AND EVERY FAILURE IS REPORTED, by the step's own
# status and then by looking, because an `rm -f` that was refused traversal
# fails while the existence check answers nothing. The marker stays until BOTH
# the registration and the token file are gone, so a later cleanup (or a
# person) finds a service token that stayed; a delete that finds the
# registration already gone counts as gone, which is what a retry meets.
set -u

failed=0

for f in "$RUNNER_TEMP/billet-ssh-key" "$RUNNER_TEMP/billet-app-key.pem" "$RUNNER_TEMP/billet-child-env" "$RUNNER_TEMP/billet-child-env-empty"; do
  if ! rm -f "$f" || [[ -e $f ]]; then
    echo "::error::$f could not be removed"
    failed=1
  fi
done

if [[ -f "$RUNNER_TEMP/billet-warp-registered" ]]; then
  registration_gone=0
  token_gone=0
  timeout -k 5 60 warp-cli --accept-tos disconnect || echo "::warning::warp-cli disconnect failed; continuing with the registration"
  if output=$(timeout -k 5 60 warp-cli --accept-tos registration delete 2>&1); then
    registration_gone=1
  elif grep -qiE 'registration missing|missing registration|not registered|no registration' <<<"$output"; then
    echo "the registration is already gone"
    registration_gone=1
  else
    echo "::error::warp-cli registration delete failed; the registration this run made may still exist on this runner: $output"
    failed=1
  fi
  if sudo rm -f /var/lib/cloudflare-warp/mdm.xml; then
    token_gone=1
  else
    echo "::error::/var/lib/cloudflare-warp/mdm.xml could not be removed; the service token stays on this runner"
    failed=1
  fi
  if [[ $registration_gone == 1 && $token_gone == 1 ]]; then
    if ! rm -f "$RUNNER_TEMP/billet-warp-registered" || [[ -e "$RUNNER_TEMP/billet-warp-registered" ]]; then
      echo "::error::the ownership marker $RUNNER_TEMP/billet-warp-registered could not be removed"
      failed=1
    fi
  else
    echo "::error::the marker $RUNNER_TEMP/billet-warp-registered is kept, so a later cleanup knows what this run left"
  fi
fi

exit "$failed"
