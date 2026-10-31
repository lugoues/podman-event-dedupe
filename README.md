# podman-event-dedupe

A small proxy between Traefik and the Podman API socket that stops Podman's
healthcheck events from making Traefik re-list every container on every
healthcheck run.

## Why

Traefik's Docker provider re-lists and inspects all containers whenever it sees
a `start`, `die` or `health_status*` event. Docker only sends `health_status`
when a container's health *changes*. Podman sends it on *every* healthcheck run
([podman#24003](https://github.com/podman-container-tools/podman/issues/24003)).
With a few containers on short intervals, Traefik hammers the Podman socket
continuously.

The proxy passes the Docker-compatible API through untouched, except
`/events`, where it forwards a container's `health_status` event only when the
status changes (plus one follow-up, see `--repeat-after-transition`). That is
Docker's behaviour, so Traefik works as designed.

Podman's own `/libpod/events` endpoint is never filtered.

When Podman ships its opt-in change-only setting
([podman#30076](https://github.com/podman-container-tools/podman/pull/30076),
still a draft as of 2026-10-31), use that instead.

## Configuration

Every flag can also be set with the environment variable shown.

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--listen` | `PED_LISTEN` | `unix:///run/ped/docker.sock` | `unix://` or `tcp://` address to listen on |
| `--listen-socket-mode` | `PED_SOCKET_MODE` | `0660` | Permissions of the unix listen socket |
| `--upstream` | `PED_UPSTREAM` | `unix:///run/podman/podman.sock` | Podman API socket |
| `--mode` | `PED_MODE` | `readonly` | `readonly` or `passthrough` |
| `--repeat-after-transition` | `PED_REPEAT` | `1` | Same-status events forwarded after a change |
| `--resync-interval` | `PED_RESYNC` | `0` (off) | Forward at most one otherwise-dropped health event per interval |
| `--max-event-bytes` | `PED_MAX_EVENT` | `4MiB` | Events larger than this are forwarded unparsed |
| `--log-level` | `PED_LOG` | `info` | `debug`, `info`, `warn`, `error` |

**`readonly` mode** allows only `GET`/`HEAD` on `/_ping`, `/version`,
`/events`, `/containers/json` and `/containers/{id}/json` (optionally prefixed
with `/vX.Y`), which is everything Traefik's Docker provider calls. Anything
else gets a `403`.

> Read-only does not mean harmless. Container inspect returns every
> container's environment variables, which often hold secrets. Read-only mode
> protects the host from changes, not data from being read.

## Deployment

Build the image:

```sh
podman build -t localhost/podman-event-dedupe:latest .
```

Example rootless Quadlet units are in [`deploy/quadlet/`](deploy/quadlet/).
Copy them to `~/.config/containers/systemd/`, enable the Podman socket and
start the services:

```sh
systemctl --user enable --now podman.socket
cp deploy/quadlet/* ~/.config/containers/systemd/
systemctl --user daemon-reload
systemctl --user start podman-event-dedupe traefik
```

For rootful Podman, change the socket mount to `/run/podman/podman.sock`.

Traefik reads the proxy's socket from the shared `ped` volume:
`--providers.docker.endpoint=unix:///run/ped/docker.sock`.

### SELinux

`podman-event-dedupe.container` sets `SecurityLabelDisable=true`. Tested on
Fedora 44 (enforcing): without it the proxy gets `permission denied`
connecting to the Podman socket, which is labelled `user_tmp_t` (rootless).
Relabelling the socket with `:z` is not an option, because it would relabel
your live Podman socket on the host. Traefik does not need this: it stays
confined and connects to the proxy socket in the shared volume without
problems.

The proxy process runs as root inside the container (`User=0`). In rootless
mode that maps to your own user, which owns the Podman socket. Access to the
Podman socket is root-equivalent (for that user) anyway.

### TCP alternative

[`deploy/quadlet-tcp/`](deploy/quadlet-tcp/) has the proxy listen on
`tcp://0.0.0.0:2375` on an internal Podman network (`Internal=true`, no
published port) and points Traefik at `tcp://podman-event-dedupe:2375`.
No shared volume is needed, but **any container attached to that network can
reach the proxy**, and with it the Podman API (read-only by default). Keep
only Traefik on it.

## Checking that it works

- `curl --unix-socket <proxy.sock> http://d/_ped/stats` returns counters:
  `events_in`, `events_forwarded`, `health_dropped`, `streams_active`.
  `health_dropped` should climb with every healthcheck run.
- `--log-level debug` logs every forward/drop decision with container name,
  status and reason.
- With Traefik at `--log.level=DEBUG`, `Provider event received` lines should
  only appear for starts, stops and health changes.

## Releases

Pushing a `v*` tag runs CI, then publishes:

- `linux/amd64` and `linux/arm64` binaries plus `checksums.txt` to a GitHub
  release (tags with a `-`, like `v1.0.0-rc.1`, are marked pre-release);
- a multi-arch image to `ghcr.io/<owner>/podman-event-dedupe:<version>` and
  `:<major>.<minor>`.

Both have build provenance attestations:

```sh
gh attestation verify podman-event-dedupe_v1.0.0_linux_amd64.tar.gz --owner <owner>
gh attestation verify oci://ghcr.io/<owner>/podman-event-dedupe:1.0.0 --owner <owner>
```

## Development

Tooling is managed with [mise](https://mise.jdx.dev):

```sh
mise run lint    # golangci-lint, shellcheck, actionlint, zizmor
mise run test    # go test -race ./...
mise run build   # static binary in dist/
```

CI runs the same tasks on every push and pull request.

Dependencies (actions, base images, mise tools) are updated weekly by the
[Renovate app](https://github.com/apps/renovate), configured in
[`.github/renovate.json5`](.github/renovate.json5). Updates wait 7 days after
release.

## End-to-end testing

[`test/e2e/e2e.sh`](test/e2e/e2e.sh) runs against real rootless Podman and
Traefik: `checks` verifies routing behaviour through the proxy (new, unhealthy,
recovered, stopped containers; proxy and Podman restarts), and `load`
counts Traefik provider events and Podman API CPU time with or without the
proxy.

### Results

Fedora 44 VM under QEMU **software emulation** (no KVM, 4 vCPU), rootless
Podman 5.8.1, Traefik 3.7.17, 10 backends with `--health-interval 2s`,
5 minutes each:

| | Traefik `Provider event received` | Podman API CPU time |
|---|---|---|
| Direct to Podman | 70 | 93.2 s |
| Through the proxy | 20 | 35.1 s |

With the proxy, the 20 events are each backend's first health event plus one
repeat after Traefik connected; the count stays flat after that. The direct
count is limited by how fast Traefik can re-list under emulation (each event
blocks it for several seconds); on real hardware it would track the
healthcheck rate (~1500 runs in 5 minutes). Under emulation only the ratio is
meaningful, not absolute numbers.

`e2e.sh checks` passed every step. Route changes took 4 to 30 s, dominated
by the emulated VM (load average 13 to 18), where 2 s healthchecks actually
ran every 7 to 9 s. Both Quadlet variants were started and routed a new
container in about 4 s.
