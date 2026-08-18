#!/usr/bin/env bash
set -euo pipefail

safe_job=$(printf '%s' "${GITHUB_JOB:-job}" | tr -cs 'a-zA-Z0-9_.-' '-')
identity="${GITHUB_RUN_ID:-0}-${GITHUB_RUN_ATTEMPT:-0}-${safe_job}"
container="billet-buildkit-${identity}"
builder="billet-${identity}"
state="${RUNNER_TEMP}/billet-buildkit-state"
socket_dir="${RUNNER_TEMP}/billet-buildkit-socket"
config="${RUNNER_TEMP}/billet-buildkitd.toml"

if ! docker buildx version >/dev/null 2>&1; then
  echo "docker buildx is required by setup-docker-builder; install the Docker Buildx CLI plugin (Billet guest images include the Ubuntu docker-buildx package)" >&2
  exit 1
fi

mkdir -p "$socket_dir"
case "${BILLET_BUILDKIT_RESET:-false}" in
  false) ;;
  true)
    if [[ "$state" != "${RUNNER_TEMP}/billet-buildkit-state" || ! -d "$state" ]]; then
      echo "refusing to reset BuildKit state outside its mounted runner-temp directory" >&2
      exit 1
    fi
    find "$state" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
    ;;
  *)
    echo "reset must be true or false" >&2
    exit 1
    ;;
esac

cat > "$config" <<'EOF'
[worker.oci]
  gc = true

  [[worker.oci.gcpolicy]]
    keepDuration = 691200
    keepBytes = 85899345920

  [[worker.oci.gcpolicy]]
    keepBytes = 96636764160
EOF

registry_mirrors=${BILLET_REGISTRY_MIRRORS_JSON:-}
if [[ -n "$registry_mirrors" ]]; then
  if jq -e '
    def origin:
      type == "string" and
      test("^https://[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?(?::[1-9][0-9]{0,4})?$") and
      (if test(":[0-9]+$") then (split(":")[-1] | tonumber) <= 65535 else true end);
    type == "object" and
    keys == ["docker.io", "ghcr.io", "quay.io"] and
    all(.[]; origin) and
    ([.[]] | unique | length) == 3
  ' >/dev/null 2>&1 <<<"$registry_mirrors"; then
    for upstream in docker.io ghcr.io quay.io; do
      endpoint=$(jq -r --arg upstream "$upstream" '.[$upstream]' <<<"$registry_mirrors")
      mirror=${endpoint#https://}
      {
        printf '\n[registry."%s"]\n' "$upstream"
        printf '  mirrors = ["%s"]\n' "$mirror"
      } >> "$config"
    done
  else
    echo "::warning::billet ignored invalid registry-mirror metadata; BuildKit will pull directly upstream"
  fi
fi

docker run --detach --privileged \
  --name "$container" \
  --volume "$state:/var/lib/buildkit" \
  --volume "$socket_dir:/run/buildkit" \
  --volume "$config:/etc/buildkit/buildkitd.toml:ro" \
  "$BILLET_BUILDKIT_IMAGE" \
  --config /etc/buildkit/buildkitd.toml \
  --addr unix:///run/buildkit/buildkitd.sock >/dev/null

for _ in $(seq 1 120); do
  if docker exec "$container" buildctl --addr unix:///run/buildkit/buildkitd.sock debug workers >/dev/null 2>&1; then
    break
  fi
  sleep 0.25
done

if ! docker exec "$container" buildctl --addr unix:///run/buildkit/buildkitd.sock debug workers >/dev/null 2>&1; then
  docker logs "$container" >&2 || true
  exit 1
fi

docker buildx create --name "$builder" --driver remote "unix://${socket_dir}/buildkitd.sock" --use >/dev/null
docker buildx inspect --bootstrap "$builder" >/dev/null

{
  echo "name=$builder"
  echo "container=$container"
} >> "$GITHUB_OUTPUT"
