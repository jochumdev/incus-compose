# AGENTS.md

AI-specific and meta rules for working in this repository.

This project is destined for the **lxc** org, so the org-wide agent rules below
(adapted from `lxc/incus` `AGENTS.md`) apply and take precedence.

To get an idea about the project read [README.md](README.md).

## Rule hierarchy

1. The org rules in this file (Legal, Formatting) - non-negotiable.
2. [CONTRIBUTING.md](CONTRIBUTING.md) - canonical coding, architecture, testing,
   and workflow rules; recursively read the docs it references.
3. `AGENTS.local.md` - personal collaboration notes (untracked, local only).

Resolve conflicts upward: org rules beat CONTRIBUTING.md, which beats local notes.
Do not restate or reinterpret project rules locally. Everything not fixed here is
discussable - always ask before guessing.

## Legal

- All contributions to this repository must be compatible with the Apache 2.0 license.
- Specifically (but not limited to), contributions cannot include code licensed under the terms of the GPL, AGPL or LGPL licenses.
- Only human beings are allowed to sign the Developer Certificate of Ownership (DCO / Signed-off-by).
- Only human beings can ever be credited within commit messages.

## Formatting

- Code comments should be no longer than one line, unless they are required to cover complex unintuitive logic.
- Never explain previous behaviour in comments.
- Comments are not safeguards, they are informal. An API is safe to use from several goroutines because it is mutex-free or confined to one, never because a comment says it is.
- Commit messages should similarly be kept as short and to the point as possible, no need to summarize the whole issue. Keep the conventional `<type>(<scope>): <description>` format from CONTRIBUTING.md.
- Do not use `go vet`, `gci` or any of those diagnostics tools, use gopls.
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

This project's testing model differs from the org default, so the org
testing rules do not apply here. Follow this repo's own rules in
[CONTRIBUTING.md](CONTRIBUTING.md) and
[docs/architecture/testing.md](docs/architecture/testing.md). Use `just`
commands instead of raw `go` (see `just --list`).

## Working in this repo

- Check existing patterns in the codebase before creating new ones.
- In most cases we do not enforce security by comments, we enforce by code and architecture.
- Think through framework/library behavior before coding.
- Keep code direct - no unnecessary intermediate variables; use `_` for unused parameters.
- If cycling (same approach, no progress), stop and ask.
- Removing user-visible output or an exported symbol is its own announced change, never folded into a cleanup.
- Run long commands (test suites, builds, `up`) in the background so the terminal stays usable, and report when they exit.
- Never chain edit -> test -> restore in one shell invocation. Interrupted or denied mid-chain the edit lands and the restore never runs; keep each step separately reversible.
- Before changing behaviour that contradicts the upstream docs, check them (`~/vendor/go/incus/doc/`). If we deviate anyway, record why in the code - the next reader will otherwise "fix" it back.

## gopls

Reach for the gopls MCP tools before grep. They answer from the type checker, so
they know what a symbol _is_, not what its name looks like. Underusing them is
the most common way an agent wastes a turn here.

| Tool                   | Use it for                                                                       |
| ---------------------- | -------------------------------------------------------------------------------- |
| `go_diagnostics`       | build and analysis errors, **with fix diffs**. After every edit.                 |
| `go_search`            | find a symbol by fuzzy name, when you do not know where it lives                 |
| `go_file_context`      | what a file uses from the rest of its package. After reading one the first time. |
| `go_package_api`       | the public API of a package, ours or a dependency - beats reading its source     |
| `go_symbol_references` | every use of a symbol. Before changing its signature or deleting it.             |
| `go_rename_symbol`     | rename across the workspace                                                      |
| `go_vulncheck`         | after touching `go.mod`                                                          |
| `go_workspace`         | module layout, once per session                                                  |

### After every Go edit

```
go_diagnostics({"files": ["/abs/path/to/edited.go"]})
```

It returns each error with a suggested patch. Apply the patch rather than
working the change out yourself, then call it again to confirm. It is
workspace-wide, so it also catches what an edit broke in a file you did not
touch - and it is faster than `just fix`, which cannot report a missing import
at all (the typecheck fails before the formatters run).

**This is how you fix a missing import.** The diff it hands back names the right
module, because gopls resolves against the module graph. Do not reconstruct the
import path by hand and do not reach for `goimports`.

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
