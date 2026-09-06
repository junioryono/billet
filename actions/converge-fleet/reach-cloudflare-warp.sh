#!/usr/bin/env bash
# Join the Zero Trust network as a headless WARP client enrolled by a service
# token: the CI half of Route D in docs/deploying/reaching-hosts.md, whose
# Cloudflare half is terraform/modules/converge-cloudflare-warp.
#
# The device authenticates as the shared non_identity@<team>.cloudflareaccess.com
# principal, whose only grant is the one Gateway rule the module writes.
#
# THE SIGNING KEY IS VERIFIED AGAINST THE FINGERPRINT THE COLLECTION PINS, read
# from the warp_connector role's defaults in the same checkout, so the action
# and the role cannot trust two different keys. The package installs root
# services that outlive this step and every later step holds a secret, so a
# substituted key here would be a substituted key everywhere.
#
# mdm.xml IS WRITTEN FROM STDIN WITH install -m 0600, so the secret never lands
# in argv (/proc/<pid>/cmdline is world-readable, /proc/<pid>/environ is not)
# or in a world-readable file, and the daemon is restarted afterwards because
# the client reads the file once at startup.
#
# THE RUNNER MUST HOLD NO REGISTRATION BEFORE THIS RUNS. Cleanup deletes the
# registration this run made, and the only way to know a registration is this
# run's is to have proved there was none before; a runner that already has one
# (a deploy runner, a laptop) is refused rather than adopted. The marker is
# written the moment a registration can exist, before the daemon restarts, so
# a registration that was created and never connected is still deleted.
#
# EVERY CALL IS BOUNDED. curl has a deadline, each warp-cli call has one, and
# the connect loop ends on a clock as well as on a count, because one call that
# never returned would otherwise hold the job until its timeout.
set -euo pipefail

if [[ -z ${CF_CLIENT_ID:-} || -z ${CF_CLIENT_SECRET:-} || -z ${CF_TEAM:-} ]]; then
  echo "::error::reach cloudflare-warp needs cloudflare-team-name, cloudflare-service-token-id and cloudflare-service-token-secret; the module's outputs are the two halves of the token"
  exit 1
fi

checkout=$(cd "$GITHUB_ACTION_PATH/../.." && pwd)
defaults="$checkout/ansible_collections/junioryono/billet/roles/warp_connector/defaults/main.yml"
key_fpr=$(sed -n 's/^billet_warp_signing_fingerprint: *//p' "$defaults" | tr -d '[:space:]"'"'")
if [[ ! $key_fpr =~ ^[0-9A-F]{40}$ ]]; then
  echo "::error::$defaults names no 40-hex billet_warp_signing_fingerprint (read '$key_fpr'); the role's pin is what this action trusts"
  exit 1
fi

# A registration already on this runner is somebody's; refuse before anything
# is written, because cleanup would delete it.
if command -v warp-cli >/dev/null 2>&1; then
  if existing=$(timeout 30 warp-cli --accept-tos registration show 2>/dev/null) && [[ -n $existing ]] && ! grep -qi 'missing' <<<"$existing"; then
    echo "::error::this runner already holds a WARP registration, so the action cannot enrol one it can later delete without deleting somebody's. Use a runner with no registration, or reach: none on a runner that already reaches the hosts."
    exit 1
  fi
fi

key="$RUNNER_TEMP/cloudflare-warp.asc"
curl -fsSL --max-time 60 --proto '=https' --proto-redir '=https' https://pkg.cloudflareclient.com/pubkey.gpg -o "$key"
keys=$(gpg --show-keys --with-colons "$key")
pubs=$(printf '%s\n' "$keys" | grep -c '^pub:' || true)
fpr=$(printf '%s\n' "$keys" | awk -F: '/^fpr:/ { print $10; exit }')
if [[ $pubs != 1 || $fpr != "$key_fpr" ]]; then
  echo "::error::The Cloudflare WARP signing key is not exactly one primary key with the pinned fingerprint $key_fpr. Refusing to add the repository: either the download was intercepted or Cloudflare rotated the key; confirm against pkg.cloudflareclient.com before updating the pin in the warp_connector role."
  exit 1
fi
sudo gpg --yes --dearmor -o /usr/share/keyrings/cloudflare-warp-archive-keyring.gpg "$key"
echo "deb [signed-by=/usr/share/keyrings/cloudflare-warp-archive-keyring.gpg] https://pkg.cloudflareclient.com/ $(lsb_release -cs) main" |
  sudo tee /etc/apt/sources.list.d/cloudflare-client.list >/dev/null
sudo timeout 300 apt-get update -qq
sudo timeout 600 apt-get install -y -qq cloudflare-warp

sudo mkdir -p /var/lib/cloudflare-warp
printf '%s\n' \
  '<dict>' \
  '  <key>organization</key><string>'"$CF_TEAM"'</string>' \
  '  <key>auth_client_id</key><string>'"$CF_CLIENT_ID"'</string>' \
  '  <key>auth_client_secret</key><string>'"$CF_CLIENT_SECRET"'</string>' \
  '  <key>service_mode</key><string>warp</string>' \
  '  <key>auto_connect</key><integer>1</integer>' \
  '  <key>onboarding</key><false/>' \
  '</dict>' |
  sudo install -m 0600 -o root -g root /dev/stdin /var/lib/cloudflare-warp/mdm.xml
# THE OWNERSHIP MARKER, before the daemon can register: the runner held no
# registration a moment ago, so any registration from here on is this run's.
: >"$RUNNER_TEMP/billet-warp-registered"

sudo systemctl restart warp-svc
sleep 3
timeout 60 warp-cli --accept-tos connect || true
connected=
deadline=$((SECONDS + 90))
for _ in $(seq 1 30); do
  if timeout 30 warp-cli --accept-tos status 2>/dev/null | grep -q 'Connected'; then
    connected=1
    break
  fi
  if (( SECONDS >= deadline )); then
    break
  fi
  sleep 2
done
timeout 30 warp-cli --accept-tos status || true
if [[ -z $connected ]]; then
  echo "::error::The WARP client did not reach Connected within its bound (30 tries or 90s). A device that registered and cannot connect is usually the enrolment policy: the module's enrollment_policy_id is not attached to the WARP enrolment application, or the token expired. It is not the Gateway rule, which decides what an enrolled device reaches."
  exit 1
fi
