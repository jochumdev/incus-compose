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
notes. A lower layer may add guidance only where the higher layer is silent; it
may not weaken or contradict a higher layer.

Keep project policy in CONTRIBUTING.md. This file adds AI-specific execution
rules and repository-specific clarifications. Everything not fixed here is
discussable - always ask before guessing.

## Legal

Licensing is in [CONTRIBUTING.md](CONTRIBUTING.md). What is specific to you:

- Only human beings are allowed to sign the Developer Certificate of Ownership
  (DCO / Signed-off-by).
- Only human beings can ever be credited within commit messages.

## Formatting

- Follow the comment rules in CONTRIBUTING.md. In addition to required exported
  doc comments, do not write organizational comments or comments that merely
  summarize the code; add comments for non-obvious reasons and constraints.
- Commit messages are kept as short and to the point as possible, no need to
  summarize the whole issue. Keep the conventional
  `<type>(<scope>): <description>` format from CONTRIBUTING.md.
- Do not use `go vet`, `gci` or any of those diagnostics tools, use `just fix`.
- You don't need to capture tests on your own use `just test-log` to get the
  last log.
- We don't use the define and test one line `if` syntax, instead splitting
  definition and testing across two lines:

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

## Architectural decisions

Six rules. A1 is why the rest exist; most of them push in one direction: fewer
hops and plainer types.

### A1. Code is written once and read many times

Keep it simple, and follow the lead of whoever is directing the work - they sign
it, so their judgement decides. A helper is a hop: a frame the reader has to
hold on the way to the thing they came for. Inline helpers that only shorten one
caller or have one call site, unless the caller would stop being readable. The
same judgement applies to large functions: stop extracting when the code becomes
harder to follow.

### A2. Constraints outside, freedom inside

Preconditions belong at the boundary, and the body is then free to be direct.
Hops are the cost, not lines: a long function is fine, but a function defending
itself against its own callers is not. Checking again in the body re-decides
what the boundary already decided, and two places defining what is valid will
drift. It nests: an outer boundary constrains an inner one, and each body is
free inside what it was handed.

### A3. One struct, one domain

A type holding two unrelated concerns is a design error rather than an untidy
one; "too many fields" is how it shows up. Each field should have a clear domain
and ownership.

### A4. Sharing data is expensive

One owner per piece of data, and when it has to cross, the least of it that will
do. Every field that crosses is a field whose ownership and failure mode
somebody has to settle, and keep settled. A mutex, or a doc comment naming an
owner the code contradicts, is not an ownership boundary. Hand over a value that
says what changed - across goroutines, where this is sharpest, and between two
structs that both hold a copy.

### A5. A function does one job, including construction

Build clients at the caller or wiring boundary, where ownership and closing are
clear. Do not make a function return a client when its job is something else,
and avoid fields or lazy getters whose only purpose is deferred construction.

### A6. Abstractions are the author's call

An interface, a plugin seam, a layer - each is a bet on what will vary, and what
settles it is where the project is going, which is not in the tree. Adding one
is a decision for whoever is directing the work. Taking one away is welcome
where it is genuinely unearned, but "I cannot see what this is for" is not the
same finding as "this is for nothing", and the difference is one question.

## Testing

This project's testing model differs from the org default, so the org testing
rules do not apply here. Follow this repo's own rules in
[CONTRIBUTING.md](CONTRIBUTING.md) and
[docs/root/architecture/testing.md](docs/root/architecture/testing.md).

## Working in this repo

- Check existing patterns in the codebase before creating new ones.
- Do not invoke raw `go build` or `go run`; use `just` for compilation and
  execution. `just run <args>` builds from the tree and runs it, which is what
  you want; it costs seconds, so run the thing rather than reasoning about it.
  `just build` recreates the sidecar and rewrites `.env`, so it destroys the
  state you are debugging - reach for it only when you mean to.
- Think through framework/library behavior before coding.
- Keep code direct - no unnecessary intermediate variables.
- If cycling (same approach, no progress), stop and ask.
- Removing user-visible output or an exported symbol is its own announced
  change, never folded into a cleanup.
- Run long commands (test suites, builds, `up`) in the foreground; several
  agents share one host, see `AGENTS.local.md`.
- Never chain edit -> test -> restore in one shell invocation. Interrupted or
  denied mid-chain the edit lands and the restore never runs; keep each step
  separately reversible.
- A test instance that has to stay up runs `oci.entrypoint: sh`. It needs no
  `sleep`, and the test does not wait for one.
- Before changing behaviour that contradicts the upstream docs, check them
  (`~/vendor/go/incus/doc/`). If we deviate anyway, record why in the code - the
  next reader will otherwise "fix" it back.

## Navigation

**Navigate with `rg`.** Measured in this repo: `rg` beats `grep` for finding and
counting. Use it to locate a symbol, list its call sites, or sweep a package -
and do not ask first. `work/` is gitignored, so it is skipped silently.

A bare `\bname\(` count merges methods sharing a name across types - this repo
has `start`, `stop`, `log`, `sort`, `all` and `equals`. Include the receiver, or
count declarations first, when the name is generic.

Renaming a symbol is `sed` over `git ls-files '*.go'` with word boundaries, and
`just lint <path>` afterwards to catch what it missed.

### After every Go edit

`just fix <path>`. It reports a broken import as `typecheck`, so a package that
lints clean also compiles. Scope it: whole-tree and single-package runs cost the
same few seconds here, and a whole-tree run from one worktree can hand a stale
answer to the next.

## Changing ic-healthd

The sidecar runs a container image, so a change under `cmd/ic-healthd/**` - or
in what it compiles in, `shared/`, `iclient/` and the dependencies - reaches it
only after `just update-healthd`, which also points
`INCUS_COMPOSE_HEALTHD_IMAGE` in `.env` at the new tag. Change none of those and
there is nothing to rebuild.

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

The sidecar's `limits.cpu`/`limits.memory` are charged against the user's
project quota, so raising them can break projects that fit before.

## Claude agents

Use `.claude/settings*.json` (permissions, deny list) and `.claude/commands/*`.
Run `/hello` to load full context.
