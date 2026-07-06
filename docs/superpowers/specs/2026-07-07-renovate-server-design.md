# Renovate Server — Design Spec

Date: 2026-07-07
Status: Approved

## Purpose

A slim, self-hosted coordinator that triggers [Renovate](https://docs.renovatebot.com/)
runs for repositories on GitLab (and GitHub) in response to webhook events and cron
schedules. It does not run Renovate itself in-process; it delegates each run to a
configurable executor (GitLab pipeline trigger, Kubernetes Job, or Docker container)
and guarantees that a given repository never has more than one run in flight.

Primary use case: a private GitLab instance with a top-level group. A single group
webhook covers all subgroups and projects. When someone ticks Renovate's rebase
checkbox on an MR, or a dependency-dashboard checkbox on an issue, the server triggers
a Renovate run scoped to that repository.

This is a from-scratch rewrite. The abandoned `arhat.dev/renovate-server` project
(2021) served as a concept reference (debounce queue, checkbox detection, cron
fallback, executor interface) but none of its code is reused.

## Requirements

1. Listen for GitLab group webhooks (merge request, issue, optionally push) and
   GitHub org webhooks; validate secrets; map events to affected repositories.
2. Trigger Renovate per repository via pluggable executors:
   - `gitlabPipeline`: call the pipeline trigger API on a central runner project
     with variables (e.g. `RENOVATE_REPO`), poll pipeline status for completion.
   - `kubernetes`: create a Job running the Renovate image with per-repo env,
     watch for completion, re-adopt running Jobs after server restart via labels.
   - `docker`: run a container against the local Docker daemon, wait for exit.
3. Per-repo mutual exclusion: never two concurrent runs for the same repository.
   Events arriving during a run coalesce into exactly one follow-up run.
4. Debounce: events within a configurable window (default 10s) merge into one run.
5. Global concurrency cap (default 4 parallel runs).
6. Run timeout (default 60m): force-release the lock, mark the run failed.
7. Cron schedules: server discovers all projects under configured groups
   (including subgroups, excluding archived) via the platform API and enqueues
   each through the same dispatch path as webhooks.
8. Failed runs are not auto-retried; the next event or cron run is the retry.
9. Caching support for k8s/docker executors: shared volume (PVC / named volume)
   for Renovate's file cache, optional `RENOVATE_REDIS_URL` passthrough for the
   package cache. (Pipeline executor: caching is the runner's concern.)
10. YAML config file, `${ENV_VAR}` expansion for secrets, fail-fast validation.
11. State (repo run states, locks) behind a `Store` interface: in-memory
    implementation now, Redis possible later. Single replica for now.
12. Observability: `/healthz`, `/readyz`, `/metrics` (Prometheus),
    read-only `/api/v1/status` JSON (queued/running repos).
13. Solid tests: `go test -race ./...`, fakes for all interfaces, webhook fixture
    payloads, k8s fake clientset, httptest for API clients.
14. Easy deployment: multi-stage Dockerfile (static, non-root, read-only rootfs,
    distroless/scratch), Helm chart with minimal RBAC, docker-compose example.
15. CI on GitHub Actions: golangci-lint, tests with race+coverage, build,
    zizmor linting the workflows, goreleaser for releases. Renovate config for
    the repo itself.

## Architecture

Module: `github.com/BlackDark/renovate-server`. Single binary, no web framework.

```
cmd/renovate-server/     main: load config, wire components, run
internal/config/         YAML load, validation, ${ENV} expansion
internal/server/         HTTP: webhook routes, healthz/readyz/metrics/status
internal/platform/       Platform interface + Event type
  gitlab/                webhook parsing (X-Gitlab-Token), group project discovery
  github/                webhook parsing (HMAC), org repo discovery
internal/dispatch/       per-repo state machine, debounce, coalescing, semaphore
internal/store/          Store interface; memory implementation
internal/executor/       Executor interface + RunSpec
  gitlabci/              pipeline trigger + status polling
  kubernetes/            Job create/watch, label-based re-adoption
  docker/                container run/wait
internal/schedule/       cron -> discovery -> enqueue
```

### Data flow

```
webhook -> platform.ParseEvent -> Event{Repo, Reason}
cron ----> platform.DiscoverRepos ┘
                    |
                    v
          dispatch.Enqueue(repo)
                    |  debounce window (N events -> 1 run)
                    v
          state machine: idle -> queued -> running -> (pending-rerun?) -> idle
                    |  global max-parallel semaphore
                    v
          rules match repo -> executor.Start(RunSpec) -> completion watch
                    |
                    └─ done -> release lock -> pending-rerun? -> re-enqueue
```

### Core interfaces

- `Platform`: parse+authenticate webhook requests into events; discover repos.
- `Executor`: start a run for a `RunSpec` (repo, platform, env), report completion.
- `Store`: repo state transitions and lock bookkeeping (memory now, Redis later).

### Event semantics (GitLab)

- Merge request event: triggers when the description's Renovate checkbox count
  of checked items increased (rebase/retry tickle).
- Issue event: dependency dashboard issue edited with checked checkbox.
- Push event (opt-in per platform config): pushes to the default branch,
  excluding commits authored by Renovate's own git identity.

### Restart behavior

- Kubernetes executor re-adopts running Jobs on startup via labels.
- Pipeline and Docker run tracking is lost on restart; the run timeout and the
  next cron run heal any stuck locks. Accepted tradeoff for the in-memory store.

## Configuration schema

```yaml
server:
  listen: ":8080"
  log: { level: info, format: json }
  debounce: 10s
  maxConcurrentRuns: 4
  runTimeout: 60m

platforms:
  - name: company-gitlab
    type: gitlab
    baseURL: https://gitlab.company.io
    token: ${GITLAB_TOKEN}
    webhook:
      path: /webhook/gitlab
      secret: ${GITLAB_WEBHOOK_SECRET}
    events: [merge_request, issue, push]
    discovery:
      groups: [my-top-group]
      excludeArchived: true
    schedule:
      crontabs: ["0 3 * * *"]
      timezone: Europe/Berlin

executors:
  - name: ci-trigger
    type: gitlabPipeline
    platform: company-gitlab
    project: infra/renovate-runner
    ref: main
    triggerToken: ${TRIGGER_TOKEN}
    variables: { RENOVATE_REPO: "{{ .Repo }}" }
    pollInterval: 15s
  - name: k8s
    type: kubernetes
    namespace: renovate
    image: renovate/renovate:latest
    cachePVC: renovate-cache
    env: { RENOVATE_REDIS_URL: "${REDIS_URL}" }
    jobTTL: 1h
  - name: local-docker
    type: docker
    image: renovate/renovate:latest
    cacheVolume: renovate-cache

rules:              # first match wins; validation requires a catch-all
  - match: "my-top-group/legacy/**"
    disabled: true
  - match: "my-top-group/platform/**"
    executor: k8s
  - match: "**"
    executor: ci-trigger
```

Validation at startup fails fast on: unknown executor references, missing
catch-all rule, invalid globs, missing webhook secrets, unresolvable env vars.

## Error handling

- Invalid webhook signature/secret: 401, counted in metrics, no processing.
- Malformed payloads: 400; unknown event types: 200 with no action.
- Executor start failure: run marked failed, lock released, error logged and
  counted; no retry loop.
- Run timeout: lock force-released, failure metric incremented.
- Config errors: process exits non-zero at startup with a precise message.

## Security

- Constant-time comparison for GitLab webhook token; HMAC-SHA256 verification
  for GitHub signatures.
- Request body size limit on webhook endpoints.
- Secrets only via env expansion; never logged, never in the status API.
- Container: static binary, non-root UID, read-only rootfs, no shell (scratch
  or distroless/static), multi-arch (amd64/arm64).
- Helm RBAC scoped to Jobs (create/get/list/watch/delete) in one namespace.

## Testing strategy

- Dispatcher: concurrency tests for coalescing, debounce, semaphore, timeout;
  run under `-race`.
- Webhook parsers: fixture files with real GitLab/GitHub payloads.
- GitLab/GitHub API clients: `httptest` servers.
- Kubernetes executor: `client-go` fake clientset, including re-adoption.
- Docker executor: thin client interface with a hand-written fake.
- Config: table tests for validation failures and env expansion.

## CI / Release

GitHub Actions workflows: lint (golangci-lint), test (race + coverage), build,
zizmor over the workflows, goreleaser release (binaries + multi-arch images).
`.renovaterc.json` so the repo maintains itself.

## Out of scope (v1)

- Redis-backed store / multi-replica HA (interface prepared, not implemented).
- Web UI.
- Auto-retry of failed runs.
- Bitbucket/other platforms.
