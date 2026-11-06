<div align="center">

# renovate-server

**Coordinates Renovate runs across GitLab and GitHub repos.**<br>
One process takes the webhooks and the cron, and starts each run somewhere else: a GitLab pipeline, a Kubernetes Job, or a local container. A repository never has two runs at once.

[![CI](https://github.com/BlackDark/renovate-server/actions/workflows/ci.yml/badge.svg)](https://github.com/BlackDark/renovate-server/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/BlackDark/renovate-server)](https://github.com/BlackDark/renovate-server/releases/latest)
[![License](https://img.shields.io/github/license/BlackDark/renovate-server)](LICENSE)
[![Image](https://img.shields.io/badge/ghcr.io-blackdark%2Frenovate--server-2496ED)](https://github.com/BlackDark/renovate-server/pkgs/container/renovate-server)

</div>

## Why

Per-repo Renovate CI duplicates config, and two events for the same repo (a dashboard checkbox and a push) start two runs that fight over the same branches.

- **One webhook for the whole group**: a GitLab group webhook or a GitHub org webhook covers every project underneath. Repos outside `discovery.groups` are ignored.
- **One run per repo**: events that arrive during a run collapse into a single follow-up. A debounce window turns a burst into one execution.
- **Pick the runner per repo**: glob rules send each project to a central GitLab pipeline, a Kubernetes Job, or Docker. A `noop` executor lets you watch real traffic before switching a rule on.
- **Runs survive a restart** when you point the store at Redis. The default memory store needs nothing else.

## Install

```sh
cp examples/config.yaml config.yaml   # edit to match your setup
docker run -d --name renovate-server \
  -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/renovate-server/config.yaml:ro" \
  -e GITLAB_TOKEN -e GITLAB_WEBHOOK_SECRET -e PIPELINE_TRIGGER_TOKEN \
  ghcr.io/blackdark/renovate-server:latest
```

The server listens on port 8080. Point the group or org webhook at `https://<host>/<webhook.path>`. Compose file: [examples/docker-compose.yaml](examples/docker-compose.yaml). Kubernetes: the [Helm chart](deploy/chart/renovate-server).

```sh
helm install renovate-server oci://ghcr.io/blackdark/charts/renovate-server --version <version>
```

Full config, webhook setup, caching and metrics: [docs/configuration.md](docs/configuration.md).

## Features

### Triggers

- **Checkbox ticks** on a Renovate merge request or the Dependency Dashboard issue start a run. Human task lists do not.
- **Pushes to the default branch** are optional, and Renovate's own commits are skipped.
- **Cron** lists every project in the configured groups and runs each one through the same path as a webhook.

### Executors

- **gitlabPipeline**: trigger a pipeline in a central runner project and poll it to completion.
- **kubernetes**: a Job from the Renovate image. Running Jobs are picked up again after a restart.
- **docker**: a container on the local Docker daemon.
- **noop**: accept the run, log it, do nothing.

### Operations

- **Prometheus** at `/metrics`, plus `/healthz`, `/readyz`, and a read-only API for queued repos and recent runs.
- **SIGHUP** reloads routing rules and the log level.
- **Signed releases**: keyless cosign signatures, SBOM and provenance on the image, SPDX SBOMs on the archives.

## FAQ

### Does renovate-server run Renovate itself?

No. It decides that a repo should run, takes a lock, and hands the repo to an executor. Renovate's own config (platform, token, presets) is passed through that executor.

### Can one GitLab webhook cover every project?

Yes. Put the webhook on the top group. Subgroups and projects are included. `discovery.groups` is also the allowlist: events for other repos are dropped.

### Does it need Redis?

No. The memory store is the default. Queued runs are lost on restart; the next webhook or cron fills them back in. Redis keeps the queue and lets a GitLab pipeline be picked up again after a restart. Either way, run a single replica.

### How do I check a config file before deploying it?

`renovate-server -validate -config config.yaml` exits 0 when the file is valid. Every `${VAR}` in the file must be set in the environment.

## Development

Requires [mise](https://mise.jdx.dev).

```bash
git clone https://github.com/BlackDark/renovate-server && cd renovate-server
mise install
mise run check
```

`mise run build` writes a static binary. `mise run docker` builds a local image. CI runs `mise run check` and `docker build`.

Issues and PRs are welcome; run `mise run check` first.

---

<div align="center"><sub>

[Report a bug](https://github.com/BlackDark/renovate-server/issues/new) · License: [MIT](LICENSE)<br>
renovate-server is not affiliated with Renovate or Mend.

</sub></div>
