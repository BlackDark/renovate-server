# renovate-server

Coordinates Renovate runs for GitLab and GitHub. Operators run the server; it dispatches each repo to a GitLab pipeline, a Kubernetes Job, or Docker. It does not run Renovate in-process.

## Verify

`mise run check` green before every commit. CI also runs `docker build` (needs a daemon; not part of `mise run check`).

## Stack

Follows https://github.com/BlackDark/agent-presets/blob/main/stack/core.md.
Deviations:
- Go, not TypeScript: existing server, static binary, talks to the Kubernetes and Docker APIs.
- `mise run check` is `gofmt -l`, `golangci-lint fmt --diff`, `golangci-lint run`, `go vet ./...`, `go test -race ./...`. golangci-lint is the repo linter (gofmt + goimports in `.golangci.yml`). Race needs cgo.
- Releases: release-please, then goreleaser archives, the Helm chart on `ghcr.io/blackdark/charts`, and a keyless-cosigned multi-arch image with SBOM/provenance. No version file; the tag is the version. Manifest is at the last tag, `0.6.0`.
- No UI. Config is YAML validated in `internal/config`, not Zod.

## Map

- `cmd/renovate-server`: process entry (`-config`, `-validate`, `-version`).
- `internal/server`: HTTP. `internal/dispatch`: lock, coalesce, route. `internal/executor`: gitlabPipeline, kubernetes, docker, noop. `internal/platform`: GitLab and GitHub webhooks. `internal/store`: memory and Redis.
- Operator reference: `docs/configuration.md`. Annotated sample: `examples/config.yaml`.
- `docs/superpowers/`: historical design notes. Do not treat them as current instructions.

## Every change

Same PR, only what the change touches:
- tests for the changed behaviour (`go test -race`)
- `docs/configuration.md` and `examples/config.yaml` when config behavior changes
- README when a user-visible feature changes

## Code style

- Data in, data out: pure functions, side effects (I/O, time, randomness) at the edges and passed in.
- Expected failures are `error` values; panic for bugs only.
- Composition over inheritance. Methods on small types are fine.
- Small units: split a file past ~400 lines, a function past ~60.
- Names explain what, comments explain why.
- Refactors keep behaviour and land in their own commit.

## Stewardship

You own the codebase's health, not only the task: leave every touched area cleaner than you found it.
- Small fixes beside the change (dead code, stale comment or doc, missing test, unclear name): fix them in their own commit in the same PR.
- Whatever your change makes obsolete (code, flags, config, docs, deps) goes in the same PR.
- Larger problems you notice: report them to the user with `file:line` and a proposal; the PR stays on topic.

## Git

- Conventional Commits. One topic per PR.
- Merge: rebase when the commits are logically separate, squash otherwise.
- PR already merged → branch anew from `origin/main`, open a new PR.
- Force-push: `--force-with-lease` on own branches only.

## Boundaries

- `sudo`, system packages, global installs: the user runs them; ask with what and why.
- No `.github/FUNDING.yml`: ask, then copy https://github.com/BlackDark/agent-presets/blob/main/templates/FUNDING.yml
- Running user processes stay up; test on other ports.
- Language: code, comments, docs, commits in English.

## Gotchas

- One replica. The store does not coordinate across processes.
- `${VAR}` in config comes from the environment. An unset variable aborts startup.
- `deploy/chart/renovate-server/values.yaml` `image.tag` stays a tag, not a digest. Renovate has `pinDigests: false` on that file.
