# AGENTS.md

AI-specific and meta rules for working in this repository.

This project is destined for the **lxc** org, so the org-wide agent rules below
(adapted from `lxc/incus` `AGENTS.md`) apply.

To get an idea about the project read [README.md](README.md).

## Rule hierarchy

1. [CONTRIBUTING.md](CONTRIBUTING.md) - canonical coding, architecture, testing,
   and workflow rules; recursively read the docs it references.
2. This file - AI-specific rules, and the org's Legal and Formatting rules.
3. `AGENTS.local.md` - personal collaboration notes (untracked, local only).

Resolve conflicts upward: CONTRIBUTING.md beats this file, which beats local
notes. The Legal section is the one exception - it is non-negotiable and
CONTRIBUTING.md agrees with it.

Do not restate or reinterpret project rules locally. Everything not fixed here is
discussable - always ask before guessing.

## Legal

Licensing is in [CONTRIBUTING.md](CONTRIBUTING.md). What is specific to you:

- Only human beings are allowed to sign the Developer Certificate of Ownership (DCO / Signed-off-by).
- Only human beings can ever be credited within commit messages.

## Formatting

- Comments are one line. See [CONTRIBUTING.md](CONTRIBUTING.md) for the full rule; the short version is that every exported symbol gets one, a self-explanatory line gets none, and a second line needs a reason you can name.
- Commit messages are kept as short and to the point as possible, no need to summarize the whole issue. Keep the conventional `<type>(<scope>): <description>` format from CONTRIBUTING.md.
- Do not use `go vet`, `gci` or any of those diagnostics tools, use `just fix`.
- You don't need to capture tests on your own use `just test-log` to get the last log.
- We don't use the define and test one line `if` syntax, instead splitting definition and testing across two lines:

  ```go
  // Avoid
  if err := op(); err != nil {
      return err
  }

  // Prefer
  err := op()
  if err != nil {
      return err
  }
  ```

## Testing

This project's testing model differs from the org default, so the org testing
rules do not apply here. Follow this repo's own rules in
[CONTRIBUTING.md](CONTRIBUTING.md) and
[docs/root/architecture/testing.md](docs/root/architecture/testing.md).

## Working in this repo

- Check existing patterns in the codebase before creating new ones.
- `go build` and `go run` are denied, so `just` is your only compiler. `just run <args>` builds from the tree and runs it, which is what you want; it costs seconds, so run the thing rather than reasoning about it. `just build` recreates the sidecar and rewrites `.env`, so it destroys the state you are debugging - reach for it only when you mean to.
- Think through framework/library behavior before coding.
- Keep code direct - no unnecessary intermediate variables.
- If cycling (same approach, no progress), stop and ask.
- Removing user-visible output or an exported symbol is its own announced change, never folded into a cleanup.
- Run long commands (test suites, builds, `up`) in the foreground; several agents share one host, see `AGENTS.local.md`.
- Never chain edit -> test -> restore in one shell invocation. Interrupted or denied mid-chain the edit lands and the restore never runs; keep each step separately reversible.
- A test instance that has to stay up runs `oci.entrypoint: sh`. It needs no `sleep`, and the test does not wait for one.
- Before changing behaviour that contradicts the upstream docs, check them (`~/vendor/go/incus/doc/`). If we deviate anyway, record why in the code - the next reader will otherwise "fix" it back.

## Navigation

**Navigate with `rg`.** Measured in this repo: `rg` beats `grep` for finding and
counting. Use it to locate a symbol, list its call sites, or sweep a package -
and do not ask first. `work/` is gitignored, so it is skipped silently.

Renaming a symbol is `sed` over `git ls-files '*.go'` with word boundaries, and
`just lint <path>` afterwards to catch what it missed.

### After every Go edit

`just fix <path>`. It reports a broken import as `typecheck`, so a package that
lints clean also compiles. Scope it: whole-tree and single-package runs cost the
same few seconds here, and a whole-tree run from one worktree can hand a stale
answer to the next.

## Changing ic-healthd

The sidecar runs a container image, so a change under `cmd/ic-healthd/**` - or
in what it compiles in, `shared/`, `iclient/` and the dependencies - reaches it only after
`just update-healthd`, which also points `INCUS_COMPOSE_HEALTHD_IMAGE` in
`.env` at the new tag. Change none of those and there is nothing to rebuild.

A daemon acting as if your fix is not there is usually a stale image; check the
logs of healthd to make sure your version runs.

Which tests care:

- `just test-e2e ./cmd/ic-healthd/...` runs the daemon in-process, from the
  working tree. The image is never involved.
- `just test-e2e ./cmd/incus-compose/...` drives the real sidecar, so it runs
  whatever the image holds.

Iterating on the daemon itself is faster without a sidecar at all: run it on the
host and attach a project with `up --external-healthd`, or push a local binary
with `healthd up --binary`. See
[docs/root/architecture/healthd.md](docs/root/architecture/healthd.md).

Two invariants a change must not break:

- ic-healthd is the only writer of `user.healthcheck.status`. incus-compose
  writes it once at creation, with `--no-healthd`, and never otherwise.
- The scheduler loop never blocks. Anything talking to Incus runs on a pool
  worker and reports through the results channel; a blocking send there stalls
  routing and, behind it, the event stream.

The sidecar's `limits.cpu`/`limits.memory` are charged against the user's project
quota, so raising them can break projects that fit before.

## Claude agents

Use `.claude/settings*.json` (permissions, deny list) and `.claude/commands/*`.
Run `/hello` to load full context.
