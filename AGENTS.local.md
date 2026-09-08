# Personal Rules

I am the maintainer and code owner of incus-compose. Treat sessions as live
pairing sessions with a direct, practical style.

- I decide the architecture. Push back once with the reasoning, then follow my
  decision; do not re-litigate it after I restate it.
- I am the source of truth for work I describe. If the tree appears to disagree,
  point it out once, then ask for context before objecting further.
- Do not widen scope. Fix what I asked and report unrelated findings.
- Flag uncertainty and hallucinations instead of presenting guesses as facts.
- I may edit files during a task. If a tool reports a file changed, read it
  again before editing it.
- I review line by line. Nothing lands that I have not read, so do not write
  compatibility scaffolding, keep-the-old-path shims, or migration seams. If a
  change breaks callers, break them and fix them in the same commit.

## Worktree and host

- Multiple agents may work in parallel on separate worktrees. Keep tasks
  independent and run builds, tests, and `up` in the foreground; the host is
  shared and background jobs interfere with other work.
- Compile a bigger hunk to review, let me review then commit in focused chunks.
- Use `just fix <path>` while working and `just pre-commit` before handoff.
- `docs/` is a separate submodule. I make its commits myself; leave
  documentation changes there uncommitted for me.
- `.env` and the nested Incus server selection are mine. Do not repoint
  `INCUS_REMOTE` or change `.env` without asking. `just build` refreshes healthd
  and rewrites `.env`; run it when I request the refresh, not as a casual check.
- The worktree is otherwise yours: inspecting, editing, and rewriting your own
  uncommitted history is fine, but do not touch other worktrees.

## Working style

- Prefer frequent, small feedback loops and direct code.
- Inline helpers that only shorten one caller unless the caller would become
  unreadable.
- Comments are not safeguards. Make invariants enforceable in the API or state
  the non-obvious reason plainly.
- Keep documentation and issue text condensed; prefer examples over essays. Do
  not add repetitive status footers.

## Testing

Follow `docs/root/architecture/testing.md` and `CONTRIBUTING.md`; do not repeat
those documents here.

- Test production functions, not copies of their logic in test files.
- Before trusting a new test, break the fix and verify that the test goes red.
- Test failure paths and absence conditions, not only successful results. Assert
  specific errors with `require.ErrorIs` where applicable.
- Use `-count=1` when rerunning tests; use a higher count or `-race` for
  concurrent behavior.
- Do not add mocks. Tests needing Incus state use the real nested Incus; the
  existing ordering mock is the documented exception.

## Local references and handoff

Use local repository documentation before web sources:

- `docs/root/architecture/*.md` for this project
- `~/vendor/go/incus/doc/` for Incus behavior
- `~/vendor/go/incus/cmd/incus/` and `~/vendor/go/incus/client/` for upstream
  CLI and client patterns

At session handoff, report the branch, uncommitted changes, active issues, and
context that is not persisted in the repository.
