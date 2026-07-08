# Feature Ideas

Backlog of candidate features with behavior sketches and implementation
notes. Status: idea unless marked otherwise. Ordered roughly by
value-for-effort for the primary usecase (private GitLab, pipeline executor).

---

## 1. Manual trigger endpoint

**Status: in progress**

**What:** `POST /api/v1/trigger` with `{"platform": "company-gitlab", "repo": "group/app"}`
enqueues a run through the normal dispatch path (debounce, lock, rules).
Lets operators kick a repo without faking a webhook or waiting for cron.

**Implementation:**
- New config: `server.apiToken` (`${API_TOKEN}`), required to enable the
  endpoint; requests carry `Authorization: Bearer <token>` (constant-time
  compare). No token configured → 404 (endpoint absent).
- Handler validates the platform name exists and the repo passes
  `platform.RepoAllowed` against that platform's groups, then
  `dispatcher.Enqueue` with a new `ReasonManual`.
- Responses: 202 accepted / 401 / 400 unknown platform / 403 repo outside groups.
- Tests: httptest table across all outcomes.

**Effort:** S.

## 2. Config validation subcommand

**Status: in progress**

**What:** `renovate-server -validate -config config.yaml` loads + validates
the config (env expansion, rules, references) and exits 0/1 with a precise
message. For CI on the config repo and pre-reload checks.

**Implementation:** flag in `main.go`; runs `config.Load`, prints
"configuration valid" or the error. Validation logic already exists — this
only exposes it. Document `${VAR}`: unset vars fail, so CI needs dummy env
or a documented `-validate` env file convention.

**Effort:** XS.

## 3. Kubernetes pod customization

**Status: in progress**

**What:** the k8s executor currently hardcodes the Job pod except image/env/
cache. Real clusters need: resource requests/limits, nodeSelector,
tolerations, serviceAccountName, activeDeadlineSeconds, imagePullSecrets,
labels/annotations passthrough.

**Implementation:**
- Extend `config.Executor` with a `pod:` block (typed structs mirroring the
  k8s fields, kept minimal: `resources`, `nodeSelector`, `tolerations`,
  `serviceAccountName`, `imagePullSecrets`, `activeDeadlineSeconds`).
- Map onto the Job template in `buildJob`; `activeDeadlineSeconds` also acts
  as in-cluster run timeout (server timeout stays authoritative for locks).
- Tests: assert fields land in the created Job via fake clientset.

**Effort:** M.

## 4. Failure notifications

**What:** push a notification when a run fails or times out: generic webhook
(JSON POST) first; Slack/Mattermost compatible payload as a variant.

**Implementation:**
- Config: `notifications: { webhookURL: ${...}, onResults: [failure, timeout] }`.
- New `internal/notify` package implementing the dispatcher's observer
  pattern (same shape as `history.Recorder`): `Record(entry)` filters on
  result and POSTs `history.Entry` JSON with a short timeout, errors logged
  never fatal. Wire alongside history in `finish()` (make the dispatcher
  accept a list of recorders).
- Tests: httptest receiver, filter matrix, slow-endpoint timeout.

**Effort:** S–M.

## 5. Discovery config-file filter

**What:** `discovery.requireConfigFile: true` — cron only enqueues repos
that contain a renovate config file, preventing mass onboarding-MR creation
on first cron over a large group.

**Implementation:**
- GitLab: `HEAD /projects/:id/repository/files/renovate.json?ref=<default>`
  per candidate (list of filenames renovate accepts, configurable subset);
  GitHub: `GET /repos/:owner/:repo/contents/renovate.json`.
- Cache positive/negative results with TTL (e.g. 1h) to keep cron cheap.
- Alternative worth documenting instead: renovate global config
  `requireConfig: required` achieves the same with zero server code.

**Effort:** M. Decide config-side vs server-side first.

## 6. Per-rule overrides

**What:** rules gain optional overrides: `debounce`, extra `variables`
(pipeline executor), `runTimeout`. Example: monorepo group gets 30s debounce
and a bigger timeout.

**Implementation:** extend `config.Rule` + `dispatch.Route` to carry an
overrides struct; dispatcher reads per-route values with server defaults as
fallback; gitlabci merges extra variables at render time. Validation:
overrides only where the executor type supports them.

**Effort:** M.

## 7. Bounded retry with backoff

**What:** optional `retry: { attempts: 2, backoff: 5m }` per executor or
rule for transient failures (runner hiccups). Default stays 0 — cron is the
retry.

**Implementation:** dispatcher `finish()` on failure (not timeout) checks
the route's retry budget, schedules a delayed re-enqueue with attempt count
carried in the queue state. Store needs an attempts field; keep coalescing
semantics (a real event resets the budget).

**Effort:** M. Design carefully against the "requeue forever" failure mode
the old project had.

## 8. Embedded status page

**What:** `GET /ui` — single static HTML page (embedded via `go:embed`)
rendering `/api/v1/status` + `/api/v1/runs` with auto-refresh. No build
step, no framework.

**Implementation:** `internal/server/ui.go` + embedded `ui.html` (vanilla
JS fetch + table). Auth: reuse `server.apiToken` if set (query param or
basic). Strictly read-only.

**Effort:** S–M.

## 9. Multi-replica coordination

**What:** N replicas behind one Service; redis store already shared, but
debounce timers/goroutines are per-process. Needs: distributed debounce
(redis delayed set + poller), leader-election for cron (redis SETNX lease),
adoption dedup.

**Effort:** L. Only worth it when single-replica restarts become a real
availability problem — revisit after production experience.

## 10. OpenTelemetry tracing

**What:** trace webhook→enqueue→run→executor spans, propagate into pipeline
variables (`TRACEPARENT`) so runner jobs join the trace.

**Implementation:** `go.opentelemetry.io/otel` with OTLP exporter config;
spans in server middleware, dispatcher, executors. Metric parity exists, so
this is for debugging latency/flow, not alerting.

**Effort:** M–L. Wait for a concrete debugging need.

## 11. Queue-wait and event-age metrics

**What:** histograms `renovate_server_queue_wait_seconds` (enqueue→start)
and `renovate_server_event_age_seconds` (webhook receipt→enqueue) to spot
saturation of `maxConcurrentRuns`.

**Implementation:** timestamps already exist in store entries; dispatcher
observes on `StartRun`. Small metrics addition + tests.

**Effort:** S.

## 12. Image digest verification for executors

**What:** k8s/docker executors optionally verify the renovate image is
cosign-signed / pin by digest before running, mirroring the project's own
supply-chain posture.

**Implementation:** config `imageDigest:` (pin) as the simple version;
cosign verification would pull in sigstore libs — probably overkill.

**Effort:** S (digest pin) / L (verification).
