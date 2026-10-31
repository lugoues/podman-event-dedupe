#!/usr/bin/env bash
# End-to-end check against real rootless Podman and Traefik (SPEC §13).
#
#   ./e2e.sh load    MODE=proxy|direct  count Traefik provider events and Podman CPU
#   ./e2e.sh checks                     routing behaviour through the proxy
#   ./e2e.sh clean
#
# Env: N (backends, 10), INTERVAL (healthcheck, 2s), DURATION (seconds, 300),
#      IMAGE (proxy image, localhost/podman-event-dedupe:latest).
set -euo pipefail

N=${N:-10}
INTERVAL=${INTERVAL:-2s}
DURATION=${DURATION:-300}
IMAGE=${IMAGE:-localhost/podman-event-dedupe:latest}
MODE=${MODE:-proxy}
SOCK=${XDG_RUNTIME_DIR:?}/podman/podman.sock

log() { printf '%s %s\n' "$(date +%T)" "$*" >&2; }

clean() {
  podman rm -f -t0 traefik ped >/dev/null 2>&1 || true
  podman ps -aq --filter label=e2e=backend | xargs -r podman rm -f -t0 >/dev/null
}

start_proxy() {
  podman run -d --replace --name ped --user 0 --network none --security-opt label=disable \
    -v "$SOCK":/run/podman/podman.sock -v ped:/run/ped \
    "$IMAGE" --listen unix:///run/ped/docker.sock --log-level info >/dev/null
}

start_traefik() {
  local mount endpoint
  if [[ $MODE == proxy ]]; then
    mount=(-v ped:/run/ped)
    endpoint=unix:///run/ped/docker.sock
  else
    mount=(--security-opt label=disable -v "$SOCK":/var/run/docker.sock)
    endpoint=unix:///var/run/docker.sock
  fi
  podman run -d --replace --name traefik --network web -p 8080:80 "${mount[@]}" \
    docker.io/library/traefik:v3 \
    --log.level=DEBUG --entrypoints.web.address=:80 \
    --providers.docker.endpoint="$endpoint" \
    --providers.docker.exposedbydefault=false \
    --providers.docker.network=web >/dev/null
}

# backend NAME: busybox httpd that turns unhealthy when /tmp/bad exists.
backend() {
  podman run -d --replace --name "$1" --network web --label e2e=backend \
    --label traefik.enable=true \
    --label "traefik.http.routers.$1.rule=Host(\`$1.test\`)" \
    --label "traefik.http.services.$1.loadbalancer.server.port=80" \
    --health-cmd 'test ! -f /tmp/bad' --health-interval "$INTERVAL" --health-retries 1 \
    docker.io/library/busybox sh -c 'mkdir -p /www && echo ok >/www/index.html && exec httpd -f -p 80 -h /www' >/dev/null
}

http_code() { curl -s -o /dev/null -w '%{http_code}' -H "Host: $1.test" http://127.0.0.1:8080/ || true; }

# wait_for NAME CODE TIMEOUT: poll until the route returns CODE; prints seconds taken.
wait_for() {
  local start=$SECONDS
  until [[ $(http_code "$1") == "$2" ]]; do
    if ((SECONDS - start > $3)); then
      log "FAIL: $1 did not return $2 within $3s"
      return 1
    fi
    sleep 0.2
  done
  echo $((SECONDS - start))
}

podman_cpu_ns() { systemctl --user show podman.service -p CPUUsageNSec --value; }

setup() {
  clean
  podman network create --ignore web >/dev/null
  podman volume create --ignore ped >/dev/null
  [[ $MODE == proxy ]] && start_proxy
  start_traefik
  for i in $(seq 1 "$N"); do backend "c$i"; done
  wait_for c1 200 120 >/dev/null
}

load() {
  setup
  log "measuring $MODE for ${DURATION}s with $N backends at $INTERVAL"
  local since cpu0 cpu1 events
  since=$(date --iso-8601=seconds)
  cpu0=$(podman_cpu_ns)
  sleep "$DURATION"
  cpu1=$(podman_cpu_ns)
  events=$(podman logs --since "$since" traefik 2>&1 | grep -c 'Provider event received' || true)
  printf 'mode=%s events=%s podman_cpu_seconds=%.2f\n' "$MODE" "$events" "$(((cpu1 - cpu0) / 1000000))e-3"
}

failed=0

# expect DESC NAME CODE: wait up to 60s for NAME's route to return CODE.
expect() {
  local t
  if t=$(wait_for "$2" "$3" 60); then
    log "PASS $1 (${t}s)"
  else
    failed=1
  fi
}

checks() {
  MODE=proxy
  setup

  backend cnew
  expect "new container routed once healthy" cnew 200

  podman exec cnew touch /tmp/bad
  expect "unhealthy container lost its route" cnew 404
  podman exec cnew rm /tmp/bad
  expect "recovered container routed again" cnew 200

  podman stop -t0 cnew >/dev/null
  expect "stopped container lost its route" cnew 404

  podman restart -t0 ped >/dev/null
  podman start cnew >/dev/null
  expect "after proxy restart, new route appears" cnew 200

  systemctl --user restart podman.service
  podman stop -t0 cnew >/dev/null
  expect "after podman service restart, stopped route removed" cnew 404
  expect "existing routes intact" c1 200
  return $failed
}

case ${1:-} in
  load) load ;;
  checks) checks ;;
  clean) clean ;;
  *) sed -n '2,9p' "$0"; exit 2 ;;
esac
