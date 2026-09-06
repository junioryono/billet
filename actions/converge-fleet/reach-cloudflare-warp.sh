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
set -euo pipefail

if [[ -z ${CF_CLIENT_ID:-} || -z ${CF_CLIENT_SECRET:-} || -z ${CF_TEAM:-} ]]; then
  echo "::error::reach cloudflare-warp needs cloudflare-team-name, cloudflare-service-token-id and cloudflare-service-token-secret; the module's outputs are the two halves of the token"
  exit 1
fi

checkout=$(cd "$GITHUB_ACTION_PATH/../.." && pwd)
defaults="$checkout/ansible_collections/junioryono/billet/roles/warp_connector/defaults/main.yml"
key_fpr=$(sed -n 's/^billet_warp_signing_fingerprint: *//p' "$defaults" | tr -d '[:space:]')
if [[ -z $key_fpr ]]; then
  echo "::error::$defaults names no billet_warp_signing_fingerprint"
  exit 1
fi

key="$RUNNER_TEMP/cloudflare-warp.asc"
curl -fsSL --proto '=https' --proto-redir '=https' https://pkg.cloudflareclient.com/pubkey.gpg -o "$key"
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
sudo apt-get update -qq
sudo apt-get install -y -qq cloudflare-warp

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
# THE OWNERSHIP MARKER: cleanup deletes a registration only when this run made
# one, so a deploy runner's or a laptop's own registration is never deleted by
# a job that used reach none.
: >"$RUNNER_TEMP/billet-warp-registered"

sudo systemctl restart warp-svc
sleep 3
warp-cli --accept-tos connect || true
connected=
for _ in $(seq 1 30); do
  if warp-cli --accept-tos status 2>/dev/null | grep -q 'Connected'; then
    connected=1
    break
  fi
  sleep 2
done
warp-cli --accept-tos status || true
if [[ -z $connected ]]; then
  echo "::error::The WARP client did not reach Connected within 60s. A device that registered and cannot connect is usually the enrolment policy: the module's enrollment_policy_id is not attached to the WARP enrolment application, or the token expired. It is not the Gateway rule, which decides what an enrolled device reaches."
  exit 1
fi
