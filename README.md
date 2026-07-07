# renovate-server

A slim coordinator that triggers [Renovate](https://docs.renovatebot.com/) runs
for your repositories in response to webhooks and cron schedules. It does not
run Renovate itself — it delegates each run to a configurable executor and
guarantees a repository never has more than one run in flight.

## Features

- **GitLab group webhooks** (one webhook on your top group covers all subgroups
  and projects) and **GitHub org webhooks**
  - MR/PR description edits where a Renovate checkbox got ticked (rebase/retry)
  - Dependency-dashboard issue checkbox ticks
  - Optional: pushes to the default branch (excluding Renovate's own commits)
- **Cron discovery**: periodically list all projects under the configured
  groups/orgs and run each through the same pipeline as webhook events
- **Three executors**, routed per repo by glob rules:
  - `gitlabPipeline`: trigger a pipeline in a central runner project with
    variables and poll it to completion
  - `kubernetes`: spawn a Job running the Renovate image (re-adopts running
    Jobs after a server restart via labels)
  - `docker`: run a Renovate container on the local Docker daemon
- **Per-repo locking + coalescing**: events during a run collapse into exactly
  one follow-up run; N events in the debounce window become one run
- **Global concurrency cap** and **run timeout** (a stuck run cannot hold a
  repo lock forever)
- **Observability**: Prometheus `/metrics`, `/healthz`, `/readyz`,
  read-only `/api/v1/status`

## Quick start (Docker)

```sh
cp examples/config.yaml config.yaml   # edit to match your setup
docker run -d --name renovate-server \
  -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/renovate-server/config.yaml:ro" \
  -e GITLAB_TOKEN -e GITLAB_WEBHOOK_SECRET -e PIPELINE_TRIGGER_TOKEN \
  ghcr.io/blackdark/renovate-server:latest
```

Or use `examples/docker-compose.yaml`. For Kubernetes, see the
[Helm chart](deploy/chart/renovate-server).

## Configuration

YAML file, passed via `-config` (default `/etc/renovate-server/config.yaml`).
`${VAR}` references are expanded from the environment at startup; unset
variables abort startup. See [examples/config.yaml](examples/config.yaml) for
a complete annotated example.

### `server`

| Key | Default | Description |
|---|---|---|
| `listen` | `:8080` | HTTP listen address |
| `log.level` | `info` | `debug`, `info`, `warn`, `error` |
| `log.format` | `json` | `json` or `text` |
| `debounce` | `10s` | Window in which events for the same repo merge into one run |
| `maxConcurrentRuns` | `4` | Global cap on parallel runs |
| `runTimeout` | `60m` | Per-run timeout; the repo lock is force-released after it |

### `platforms[]`

| Key | Required | Description |
|---|---|---|
| `name` | yes | Unique name, referenced by executors |
| `type` | yes | `gitlab` or `github` |
| `baseURL` | gitlab: yes | Instance URL (GitHub: empty for github.com, URL for GHE) |
| `token` | yes | API token used for repo discovery |
| `botEmail` | no | Push events authored by this email are ignored |
| `webhook.path` | yes | HTTP path the webhook is served on |
| `webhook.secret` | yes | GitLab: `X-Gitlab-Token`; GitHub: HMAC secret |
| `events` | no | Subset of `merge_request`, `issue`, `push` |
| `discovery.groups` | for cron | Top groups (GitLab, incl. subgroups) or orgs (GitHub) |
| `discovery.excludeArchived` | no | Skip archived repos during discovery |
| `schedule.crontabs` | no | Standard cron expressions for periodic full runs |
| `schedule.timezone` | no | IANA timezone for the crontabs (default UTC) |

### `executors[]`

Common: `name`, `type` (`gitlabPipeline` | `kubernetes` | `docker`).

**gitlabPipeline** — `platform` (a gitlab platform name), `project` (the runner
project), `ref` (default `main`), `triggerToken`, `variables` (values are Go
templates with `{{ .Repo }}`, `{{ .Platform }}`, `{{ .Reason }}`),
`pollInterval` (default `15s`).

**kubernetes** — `namespace`, `image`, optional `cachePVC` (mounted at
`/tmp/renovate/cache`), `jobTTL` (default `1h`), `env` (extra container env).

**docker** — `image`, optional `cacheVolume` (mounted at
`/tmp/renovate/cache`), `pull` (pull image before each run), `env`.

For kubernetes/docker the server sets `RENOVATE_REPOSITORIES=<repo>`; all other
Renovate configuration (platform, endpoint, token, presets) is yours to pass
via `env` — e.g. `RENOVATE_PLATFORM`, `RENOVATE_ENDPOINT`, `RENOVATE_TOKEN`.

### `rules[]`

Ordered list, first match wins, matched against the repo's full path with
[doublestar](https://github.com/bmatcuk/doublestar) globs. Each rule either
sets `executor` or `disabled: true`. A catch-all `match: "**"` rule is
required.

## Webhook setup

**GitLab** (top group → Settings → Webhooks):
- URL: `https://<server>/<webhook.path>`, Secret token: `webhook.secret`
- Triggers: *Issues events*, *Merge request events*, optionally *Push events*

**GitHub** (org → Settings → Webhooks):
- Payload URL as above, content type `application/json`, secret = `webhook.secret`
- Events: *Issues*, *Pull requests*, optionally *Pushes*

## Caching

- File/repo cache: mount a shared volume (`cachePVC` / `cacheVolume`) —
  Renovate keeps its cache in `/tmp/renovate/cache`
- Package cache: point Renovate at Redis via `env: { RENOVATE_REDIS_URL: ... }`
  for cross-run, cross-node caching
- With the `gitlabPipeline` executor, caching is the runner's concern
  (GitLab CI cache in your runner project)

## Operations

| Endpoint | Purpose |
|---|---|
| `GET /healthz` | Liveness |
| `GET /readyz` | Readiness (200 after startup) |
| `GET /metrics` | Prometheus metrics |
| `GET /api/v1/status` | Queued/running repos as JSON |

Metrics: `renovate_server_webhook_events_total{platform,outcome}`,
`renovate_server_runs_started_total{executor}`,
`renovate_server_runs_finished_total{executor,result}`,
`renovate_server_run_duration_seconds{executor}`,
`renovate_server_repos_active`.

Restart semantics: state is in-memory (single replica). Running Kubernetes
Jobs are re-adopted on startup via labels; pipeline/docker run tracking is
lost on restart — the run timeout and the next cron run heal stuck state.
Failed runs are not auto-retried; the next event or cron run is the retry.

## Development

```sh
make lint    # golangci-lint
make test    # go test -race ./...
make build   # static binary
make docker  # container image
```

## License

[MIT](LICENSE)
