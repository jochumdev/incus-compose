# Contributing to incus-compose

Thank you for your interest in contributing! This document outlines the
conventions and practices we follow.

This project is destined for the **lxc** org. The org-wide contributing policy
([lxc/incus CONTRIBUTING](https://github.com/lxc/incus/blob/main/CONTRIBUTING.md))
applies in full, including:

- **License**: Apache 2.0, no copyright assignment. Contributions cannot include
  code licensed under the GPL, AGPL or LGPL.
- **DCO**: Every commit that lands carries a `Signed-off-by` line
  (`git commit -s`), and only a human may sign it. Work done in a maintainer's
  worktree is committed without any trailer; the maintainer signs it on the
  rebase.
- **AI tooling**: See the org policy. Contributors must fully own their work. AI
  tools cannot be credited. See also [AGENTS.md](AGENTS.md).

## Philosophy

**KISS** is about the solution: solve the problem in front of you, at the size
it actually has. Not the general case, not the one you expect next quarter. A
shallow package structure, direct code, working software over perfect
architecture.

**Boring** is about the reader: the next person should be able to guess how it
works before reading it. A plain loop over a clever pipeline, the standard
library over a dependency, the obvious name over the precise one, the pattern
already used three times in this repo over a better one used zero times. Code is
read far more often than it is written, and "boring" is what makes a diff cheap
to review at 23:00.

Neither is an excuse for a broken design. When they collide with correctness,
correctness wins - but say so, rather than quietly building the clever thing.

- No non-ASCII characters in code and docs

## Working with Go code

Follow the [Go proverbs](https://go-proverbs.github.io/).

## Architecture and design rules

Its core design principles are documented in
[docs/root/developer/index.md](docs/root/developer/index.md).

Read the documentation in your own checkout, not the published site: `docs/` is
a submodule, and a feature branch may carry a version of it that the site does
not have yet.

Before contributing, you **must** read and understand this document. It defines
non-negotiable boundaries, including:

- What incus-compose will and will not implement
- Where Compose semantics must remain untouched
- How mapping to Incus is structured
- Which layers are allowed to change behavior
- Use Incus terms and simple English

Contributions that violate these principles will be rejected, regardless of
feature completeness or test coverage.

## Project Structure

```
incus-compose/
  cmd/incus-compose/    # CLI entry point
  cmd/ic-healthd/       # Sidecar
  client/               # High-level Incus client wrapper
  iclient/              # Incus API client, a fork of the upstream one
  shared/               # Code both binaries use
  internal/             # Helpers no consumer may import
  project/              # Compose project loading and service translation
  examples/             # Example projects
  docs/                 # User-facing documentation (submodule)
  test/                 # Tests and fixtures
  just/                 # justfile modules
```

**Package Guidelines**:

- `cmd/incus-compose/` - CLI flag parsing, command handlers, wiring only
- `cmd/ic-healthd/` - Sidecar for health checking and instance restarts
- `client/` - High-level Incus wrapper, resource management, transactions
- `iclient/` - The Incus REST API, forked from `lxc/incus/client` because that
  one cannot be used from several goroutines. Everything reaches Incus through
  it; nothing else may import the upstream client
- `shared/` - Code both binaries use
- `internal/` - Helpers that must stay ours, such as `internal/testlib`
- `examples/` - Example projects ready to use with incus-compose
- `project/` - Compose-spec loading via compose-go, service-to-instance
  translation
- Root package - No code at root level (all in packages)

**Don't create**: deep nesting like `pkg/application/container/`, or abstraction
layers "for future flexibility".

## Build and Test Commands

Everything runs through `just`; use it instead of raw `go`. `just --list` shows
all of them, and
[docs/root/developer/testing.md](docs/root/developer/testing.md) documents each
in full, along with fixtures, coverage and the environment they need.

```bash
just build                  # Dev binary, and the sidecar image it is stamped with
just run <args>             # go run ./cmd/incus-compose against the .env remote
just incus <args>           # incus against the nested dev environment

just test-local [pattern]   # Unit only, no Incus needed
just test [pattern]         # Unit + integration. What CI runs
just test-e2e [pattern]     # Adds the slow full-CLI tests
just test-log [-p REGEX]    # Plain text of the newest run's log
just cover                  # Per-package and total coverage

just fix [path]             # golangci-lint --fix, scoped to a package
just pre-commit             # tidy, boundary, lint. Run before committing
```

Two that cost a round trip when missed:

- The package pattern comes **first**, `go test` flags after it.
  `just test-local -count=1` reads `-count=1` as the pattern and fails with
  `no Go files`.
- `just build` rebuilds the sidecar image. A change under `cmd/ic-healthd/`,
  `shared/` or `iclient/` reaches the running sidecar no other way.

## Code Style

### Naming

Prefer Go-style concise names over Java-style verbose names:

| Prefer     | Avoid                 |
| ---------- | --------------------- |
| `Copied()` | `IsCopiedToProject()` |
| `Status()` | `GetCurrentStatus()`  |
| `Valid()`  | `IsValidInstance()`   |
| `Backuper` | `BackupManager`       |
| `mu`       | `wellKnownMu`         |
| `err`      | `errorResult`         |

Go code reads better when names are short and context provides meaning.

Name a type for what it _is_, not for the role it plays: `Backuper`, not
`BackupManager`. `-Manager`, `-Handler`, `-Service` and `-Helper` suffixes carry
no information.

Don't qualify a variable with what it guards or holds when the scope already
says it - a mutex in a struct with one lock is `mu`.

### Helpers

Every helper is a jump, and every jump is a frame the reader has to hold while
reading the caller. Extract only what the reader never has to open: if
understanding the caller means going into the helper, inline it. A function with
one caller almost never passes that test.

### Comments

- Every exported function and type gets a doc comment: **one line**, ending with
  a period.
- A second line needs a damn good reason - a trap the caller cannot infer from
  the signature. Anything longer belongs in `docs/`, the issue, or the commit
  message.
- Delete comments that restate the code. A self-explanatory line gets no comment
  at all; that is the common case, not the exception.
- Never explain previous behaviour. That is what `git log` is for.
- Comments are not safeguards. An API is concurrency-safe because it is
  mutex-free or confined to one goroutine, never because a comment says so.

### Use of `any`

Avoid using `any` (`interface{}`). Prefer a small, explicit interface. Use
generics only if they clearly reduce duplication.

### Unused parameters

Use `_` for unused parameters rather than ignoring in the function body:

```go
// Preferred
func (t *logTerminal) Read(_ []byte) (int, error) {

// Avoid
func (t *logTerminal) Read(p []byte) (int, error) {
    _ = p
```

### CLI environment variables

- Use `INCUS_COMPOSE_*` prefix for configuration env vars
- Support common standards like [no-color.org](https://no-color.org/) where
  applicable

### Error Handling

**Return errors, don't panic**:

```go
if err != nil {
    return fmt.Errorf("creating container %s: %w", name, err)
}
```

**Aggregate errors for batch operations**:

```go
var errs error
for _, item := range items {
    err := operation(item)
    if err != nil {
        errs = errors.Join(errs, err)
    }
}
return errs
```

Note the two lines: we do not use the define-and-test one-line `if` form
anywhere, including in examples.

**Use sentinel errors**:

```go
var (
    ErrDisconnected       = errors.New("trying to use a disconnected client")
    ErrInstanceNotRunning = errors.New("the instance is not running")
)
```

**Check errors with errors.Is(), not string contains**:

```go
// Bad
if strings.Contains(err.Error(), "not found") { }

// Good
if errors.Is(err, ErrNotFound) { }
```

### Commit Messages

Use conventional commit format with package scope:

```
<type>(<package>): <description>
```

**Types**:

- `fix` - Bug fix
- `feat` - New feature or improvement
- `chore` - API or CLI API change

**Examples**:

```
fix(client): handle nil pointer in image cache
feat(cmd): add --timeout flag to up command
chore(client): rename Resource interface method
```

### Changelog

[CHANGELOG.md](CHANGELOG.md) follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Add the entry in the
same commit as the change, under the unreleased heading, in
`Added`/`Changed`/`Fixed`, ending with `(by @handle)`.

An entry is needed when a user can observe the difference:

- behaviour, CLI flags, or command output
- a bug they could have hit, even when the cause was internal

No entry for contributor docs (`AGENTS.md`, `CONTRIBUTING.md`, `docs/`), dev
tooling (`justfile`, `scripts/`), tests, refactors with no observable
difference, or a change to an exported symbol that no CLI user can see.

Write what changed for the reader, not what you did to the code - "concurrent
`up` runs no longer fail creating the same network", not "added a retry to
Ensure".

## Testing

Tiers, fixtures, coverage, and how to drive the CLI from a test are documented
in [docs/root/developer/testing.md](docs/root/developer/testing.md). Read it
before writing a test.

One policy rather than a technique, so it lives here: **do not add mocks.** A
fake `incus.InstanceServer` encodes a guess about what the daemon returns, and a
test that passes against the guess proves nothing. Anything needing Incus state
gets it from a real one. The existing ordering mock is the exception; any other
mock is a maintainer's call - ask first.

## Docker Compose Compatibility

Output should match `docker compose config` where possible.

**Intentional differences**:

- OS env vars not included by default (use `--os-env` for compatibility)
- `config --format=json` keeps `x-incus`/`x-incus-compose` (compose-go tags
  extensions `json:"-"`, so docker drops them); it therefore omits docker's
  explicit `command`/`entrypoint`/`ipam` nulls. `--format=yaml` matches exactly.

## Questions?

- Open an issue for bugs or feature requests
- Check existing documentation in `docs/`
