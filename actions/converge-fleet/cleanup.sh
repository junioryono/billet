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
#
# AND ONE THING THIS CANNOT DO, said here because the first fleet to run it met
# it immediately: a client enrolled through mdm.xml is a managed deployment and
# may not delete its own registration. That is reported with the device id and
# is not a failure — see the branch below for the measurement and the reasoning.
set -u

failed=0

for f in "$RUNNER_TEMP/billet-ssh-key" "$RUNNER_TEMP/billet-app-key.pem" "$RUNNER_TEMP/billet-child-env" "$RUNNER_TEMP/billet-child-env-empty"; do
  if ! rm -f "$f" || [[ -e $f ]]; then
    echo "::error::$f could not be removed"
    failed=1
  fi
done

if [[ -f "$RUNNER_TEMP/billet-warp-registered" ]]; then
  # SETTLED, NOT GONE: this run has nothing further to do about the registration.
  # In the managed case below the device entry provably survives, and a flag that
  # claimed otherwise would be the script telling the operator the opposite of
  # what the warning beside it says.
  registration_settled=0
  token_gone=0
  # THE DEVICE ID BEFORE ANYTHING ELSE, because it is what an operator acts on
  # and every path below can take it away. A read that fails is not a device
  # that has no id.
  device=$(timeout -k 5 30 warp-cli --accept-tos registration show 2>/dev/null |
    sed -nE 's/^[[:space:]]*Device ID:[[:space:]]*(.+)$/\1/p' | head -n1)
  device=${device:-unread}
  timeout -k 5 60 warp-cli --accept-tos disconnect || echo "::warning::warp-cli disconnect failed; continuing with the registration"
  if output=$(timeout -k 5 60 warp-cli --accept-tos registration delete 2>&1); then
    registration_settled=1
  elif grep -qiE 'registration missing|missing registration|not registered|no registration' <<<"$output"; then
    echo "the registration is already gone"
    registration_settled=1
  elif grep -qiE 'not authorized in this context' <<<"$output"; then
    # A MANAGED CLIENT MAY NOT DELETE ITS OWN REGISTRATION, and no ordering in
    # this script changes that. Measured 2026-09-19 on ubuntu-24.04 enrolled by
    # service token through mdm.xml: `registration delete` answers "Operation
    # not authorized in this context"; removing mdm.xml and restarting warp-svc
    # does NOT lift the refusal, it discards the client's local registration, so
    # the daemon returns knowing of none and the delete has nothing left to ask
    # about while the device entry survives untouched. Unmanaging first would
    # therefore report "already gone" and leak the entry silently, which is
    # worse than saying this.
    #
    # NOT A FAILURE, BECAUSE NOTHING HERE FAILED. The runner is ephemeral and
    # holds nothing after this job; what remains is the device entry, which this
    # client is not permitted to remove and which the device inactivity policy
    # expires. Failing would mark every converge of a managed fleet as failed
    # whatever the converge did, and a red run that is always red is read by
    # nobody. It is said at warning level, with the id, every time.
    echo "::warning::this WARP client is a managed deployment, so it may not delete its own registration: $output"
    echo "::warning::device $device stays in Team & Resources -> Devices until the device inactivity policy expires it. Remove it from the dashboard or the API if that is too long; a client cannot."
    registration_settled=1
  else
    echo "::error::warp-cli registration delete failed; device $device may still be registered with this Zero Trust organization: $output"
    failed=1
  fi
  if sudo rm -f /var/lib/cloudflare-warp/mdm.xml; then
    token_gone=1
  else
    echo "::error::/var/lib/cloudflare-warp/mdm.xml could not be removed; the service token stays on this runner"
    failed=1
  fi
  if [[ $registration_settled == 1 && $token_gone == 1 ]]; then
    if ! rm -f "$RUNNER_TEMP/billet-warp-registered" || [[ -e "$RUNNER_TEMP/billet-warp-registered" ]]; then
      echo "::error::the ownership marker $RUNNER_TEMP/billet-warp-registered could not be removed"
      failed=1
    fi
  else
    echo "::error::the marker $RUNNER_TEMP/billet-warp-registered is kept, so a later cleanup knows what this run left"
  fi
fi

exit "$failed"
