# renovate-server

A slim coordinator that triggers [Renovate](https://docs.renovatebot.com/) runs
for your repositories in response to webhooks and cron schedules. It does not
run Renovate itself — it delegates each run to a configurable executor and
guarantees a repository never has more than one run in flight.

## Features

- **GitLab group webhooks** (one webhook on your top group covers all subgroups
  and projects) and **GitHub org webhooks**
  - MR/PR description edits where a Renovate checkbox got ticked (rebase/retry).
    An MR counts as a Renovate MR when its description carries the
    `<!--renovate-debug:...-->` comment, its source branch matches
    `mrFilter.sourceBranchPrefixes` (default `renovate/`) or its author is in
    `mrFilter.authors`; any checkbox tick inside such an MR triggers. Ordinary
    task lists in human MRs never trigger runs (`allowAnyCheckbox` reverts this)
  - Dependency-dashboard issue checkbox ticks — the issue title
    (`dashboardIssueTitle`, default "Dependency Dashboard") identifies the
    dashboard; any checkbox tick inside it triggers
  - Optional: pushes to the default branch (excluding Renovate's own commits)
  - Repos outside the configured `discovery.groups` are rejected, so a stray
    webhook cannot trigger runs for foreign projects
- **Cron discovery**: periodically list all projects under the configured
  groups/orgs and run each through the same pipeline as webhook events
- **Three executors**, routed per repo by glob rules:
  - `gitlabPipeline`: trigger a pipeline in a central runner project with
    variables/inputs and poll it to completion
  - `kubernetes`: spawn a Job running the Renovate image (re-adopts running
    Jobs after a server restart via labels)
  - `docker`: run a Renovate container on the local Docker daemon
- **Per-repo locking + coalescing**: events during a run collapse into exactly
  one follow-up run; N events in the debounce window become one run
- **Global concurrency cap** and **run timeout** (a stuck run cannot hold a
  repo lock forever)
- **Observability**: Prometheus `/metrics`, `/healthz`, `/readyz`,
  read-only `/api/v1/status` and `/api/v1/runs` (recent run history)
- **Optional Redis store**: run state survives restarts (queued repos are
  re-enqueued at startup); memory store by default
- **SIGHUP reload** for routing rules and log level
- **Signed releases**: keyless cosign signatures + SBOMs for images and
  binaries, Helm chart published as OCI artifact

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
| `historySize` | `100` | Finished runs kept for `/api/v1/runs` |
| `store.type` | `memory` | `memory` or `redis` |
| `store.redis.url` | — | `redis://[:pass@]host:port/db`, required for redis |
| `store.redis.keyPrefix` | `renovate-server:` | Key namespace |
| `store.redis.ttl` | `2h` | Entry TTL; stale locks self-heal after it |

### `platforms[]`

| Key | Required | Description |
|---|---|---|
| `name` | yes | Unique name, referenced by executors |
| `type` | yes | `gitlab` or `github` |
| `baseURL` | gitlab: yes | Instance URL (GitHub: empty for github.com, URL for GHE) |
| `token` | yes | API token used for repo discovery |
| `botEmail` | no | Push events authored by this email are ignored |
| `dashboardIssueTitle` | no | Issues with this title are treated as the renovate dashboard: any checkbox tick triggers (default `Dependency Dashboard`, `*` = any title) |
| `allowAnyCheckbox` | no | Trigger on any checked todo item in any MR/issue; also skips the title filter |
| `mrFilter.sourceBranchPrefixes` | no | Source-branch prefixes identifying Renovate MRs (default `["renovate/"]`) |
| `mrFilter.authors` | no | MR/PR author usernames identifying Renovate MRs. GitHub: login from payload; GitLab: resolved via one cached API lookup per author |
| `webhook.path` | yes | HTTP path the webhook is served on |
| `webhook.secret` | yes | GitLab: `X-Gitlab-Token`; GitHub: HMAC secret |
| `events` | no | Subset of `merge_request`, `issue`, `push` |
| `discovery.groups` | for cron | Top groups (GitLab, incl. subgroups) or orgs (GitHub); also the webhook allowlist — events for repos outside are ignored (empty = allow all) |
| `discovery.excludeArchived` | no | Skip archived repos during discovery |
| `schedule.crontabs` | no | Standard cron expressions for periodic full runs |
| `schedule.timezone` | no | IANA timezone for the crontabs (default UTC) |

### `executors[]`

Common: `name`, `type` (`gitlabPipeline` | `kubernetes` | `docker`).

**gitlabPipeline** — `platform` (a gitlab platform name), `project` (the runner
project), `ref` (default `main`), `triggerToken`, `variables` and optional
`inputs` (values are Go templates with `{{ .Repo }}`, `{{ .Platform }}`,
`{{ .Reason }}`), `pollInterval` (default `15s`). `inputs` maps to GitLab
pipeline inputs (`spec:inputs`; GitLab 17.10+); `variables` maps to CI/CD
variables. See
[examples/renovate-runner.gitlab-ci.yml](examples/renovate-runner.gitlab-ci.yml)
for the receiving pipeline. With the Redis store, running pipelines are
re-adopted after a server restart.

**noop** — accepts runs, logs them, does nothing. Use it for a shadow-mode
rollout: point the catch-all rule at a noop executor, wire up the production
webhook, and watch `/api/v1/runs` to validate triggering against real traffic
before switching the rule to a real executor.

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
| `GET /api/v1/runs` | Recent finished runs (result, duration, error) as JSON |

Metrics: `renovate_server_webhook_events_total{platform,outcome}`,
`renovate_server_runs_started_total{executor}`,
`renovate_server_runs_finished_total{executor,result}`,
`renovate_server_run_duration_seconds{executor}`,
`renovate_server_repos_active`.

Restart semantics: with the default memory store, queued runs are lost on
restart. Running Kubernetes Jobs are re-adopted on startup via labels;
docker run tracking is lost — the run timeout and the next cron run heal
stuck state. With the Redis store, queued repos are re-enqueued at startup,
running GitLab pipelines are re-adopted (polling resumes where it left off)
and stale entries expire after `store.redis.ttl`. Failed runs are not
auto-retried; the next event or cron run is the retry. Keep `replicaCount: 1`
either way — coordination between replicas is not implemented.

Config reload: `kill -HUP <pid>` reloads `rules` and `log.level` from the
config file. Changes to `platforms`, `executors` or the listen address are
refused and require a restart.

Failed docker-executor runs log the container's last 50 log lines before the
container is removed.

## Supply chain

Images are signed with cosign (keyless) and ship buildx SBOM/provenance
attestations; release archives include SPDX SBOMs. Verify an image with:

```sh
cosign verify \
  --certificate-identity-regexp 'github.com/BlackDark/renovate-server' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/blackdark/renovate-server:<version>
```

Install the Helm chart straight from ghcr:

```sh
helm install renovate-server oci://ghcr.io/blackdark/charts/renovate-server --version <version>
```

## Development

```sh
make lint    # golangci-lint
make test    # go test -race ./...
make build   # static binary
make docker  # container image
```

## License

[MIT](LICENSE)
