---
date: 2026-08-27T23:33:35Z
dateCreated: 2026-07-05T01:03:35.484Z
description: Intercept any incus-compose resource action before or after it runs - add logging, rewrite errors, or abort the action entirely.
editor: markdown
published: true
tags: []
title: Hooks
leafwiki_id: r4mXqlBDR
leafwiki_title: Hooks
leafwiki_created_at: "2026-07-05T03:54:01Z"
leafwiki_updated_at: "2026-08-27T23:33:35Z"
leafwiki_creator_id: vOmfrlBDg
leafwiki_last_author_id: vOmfrlBDg
---

# Hooks

Hooks intercept resource actions before and after they execute.

## Overview

Every resource action (ensure, delete, start, stop) can be intercepted with
hooks:

- **Before hooks** - Run before an action starts
- **After hooks** - Run after an action completes

Hooks receive the action context and can modify errors, abort actions, or add
logging.

```mermaid
flowchart LR
    S([RunAction]) --> B1["before hook<br/>added 1st"]
    B1 --> B2["before hook<br/>added 2nd"]
    B2 --> ACT[the action]
    ACT --> A2["after hook<br/>added 2nd"]
    A2 --> A1["after hook<br/>added 1st"]
    A1 --> R([error returned to the caller])

    B1 -.->|returns an error| ABORT(["aborted - the action never runs"])
    B2 -.->|returns an error| ABORT
```

Before hooks run in the order they were added, after hooks in the reverse order,
so a pair registered together brackets everything registered after it.

Two more hooks fire once per client lifecycle rather than per action:

- **Connected hooks** - Run once when the client opens, before any action
- **Done hooks** - Run once when the client's work is complete, for cleanup

## Action Hook Signature

```go
func(ctx context.Context, action Action, r Resource, args Options, err error) error
```

**Parameters:**

- `ctx` - The context for the action
- `action` - The action being performed (ensure, delete, start, stop)
- `r` - The resource being operated on
- `args` - Action options (create, force, timeout)
- `err` - Error from previous hooks or the action

Connected and Done hooks use a smaller signature; see
[Lifecycle Hooks](#lifecycle-hooks).

**Return:**

- Modified or original error
- New error to abort the action

## Before Hooks

Before hooks run in FIFO order (first added runs first). Use them for
validation, logging, or aborting actions.

### Abort an Action

```go
client.AddHookBefore(func(_ context.Context, action Action, r Resource, args Options, err error) error {
    if action == ActionDelete && !confirmed {
        return errors.New("deletion not confirmed")
    }
    return err
})
```

### Log Action Start

```go
logger := slog.Default()

client.AddHookBefore(func(_ context.Context, action Action, r Resource, args Options, err error) error {
    logger.Info("Starting action",
        "action", action,
        "resource", r.Name(),
        "kind", r.Kind())
    return err
})
```

### Check for Previous Errors

Before hooks can see errors from earlier before hooks in the chain:

```go
client.AddHookBefore(validateResources)  // Might return error
client.AddHookBefore(func(_ context.Context, action Action, r Resource, args Options, err error) error {
    if err != nil {
        // Previous validation failed, skip this hook
        return err
    }
    // Proceed with additional checks
    return nil
})
```

## After Hooks

After hooks run in LIFO order (last added runs first). Use them for logging
results, wrapping errors, or cleanup.

### Log Action Results

```go
logger := slog.Default()

client.AddHookAfter(func(_ context.Context, action Action, r Resource, args Options, err error) error {
    if err != nil {
        logger.Error("Action failed",
            "action", action,
            "resource", r.Name(),
            "kind", r.Kind(),
            "error", err)
        return err
    }

    logger.Info("Action completed",
        "action", action,
        "resource", r.Name(),
        "kind", r.Kind())
    return nil
})
```

### Wrap Errors with Context

```go
client.AddHookAfter(func(_ context.Context, action Action, r Resource, args Options, err error) error {
    if err != nil {
        return fmt.Errorf("%s %s failed: %w", r.Kind(), r.Name(), err)
    }
    return nil
})
```

### Append Additional Errors

If your hook encounters its own error, append it to the action error:

```go
client.AddHookAfter(func(_ context.Context, action Action, r Resource, args Options, err error) error {
    if cleanupErr := cleanup(); cleanupErr != nil {
        return errors.Join(err, cleanupErr)
    }
    return err
})
```

### Attribute Failures in Complex Hooks

A long-lived after hook (for example the DNS watcher or the healthd reloader)
runs deep inside the LIFO chain, so a raw error it returns is hard to trace back
to the hook that produced it. Tag your failures with a per-hook sentinel so
`errors.Is` and logs point at the source:

```go
var ErrDNSWatcher = NewError("DNSWatcher")

client.AddHookAfter(func(ctx context.Context, action Action, r Resource, _ Options, err error) error {
    if err != nil || !r.IsEnsured() {
        return err // pass through; not our failure
    }

    ips, ipErr := r.(*Instance).WaitIPs(ctx, timeout)
    if ipErr != nil {
        return ErrDNSWatcher.Wrap(ipErr) // tag a real failure
    }
    // ...
    return nil
})
```

Now `errors.Is(err, ErrDNSWatcher)` tells you which hook failed.

**Only wrap a non-nil error.** `Wrap(nil)` returns a non-nil error (an `*Error`
with no cause), so wrapping the incoming `err` on a no-op or success path
fabricates a failure that aborts the action. Wrap only when you hold a real
error; otherwise return the incoming `err` (which may be nil):

This is the after-hook form of the "pass through errors" rule below: a hook must
never invent an error where the action actually succeeded.

## Lifecycle Hooks

Connected and Done hooks bracket the client's whole run instead of a single
action. They share a smaller signature, since there is no action or resource
yet:

```go
func(err error) error
```

They do not fire on their own. `client.Open()` fires the connected hooks (call
it once after registering all hooks, before running any stack actions), and
`client.Done()` fires the done hooks (call it when the client's work is
complete, usually deferred):

```go
if err := client.Open(); err != nil {
    return err
}
defer func() { _ = client.Done() }()
```

```mermaid
sequenceDiagram
    participant A as your code
    participant C as project Client
    participant H as hooks

    A->>C: Open()
    C->>H: connected hooks, FIFO
    Note over H: an error aborts Open()<br/>and skips the remaining ones
    A->>C: stack.Run(action)
    C->>H: before / after hooks, once per resource
    A->>C: Done()
    C->>H: done hooks, LIFO
    Note over H: all of them run,<br/>there is no short-circuit
```

### Connected Hooks

Connected hooks run in FIFO order (first added runs first). Use them for
one-time setup: starting a progress renderer, acquiring a connection-scoped
resource, or validating preconditions. Each hook starts a fresh chain (it always
receives a nil error); the only propagation is abort-on-error.

```go
client.AddHookConnected(func(err error) error {
    return progress.Start()  // Returning an error aborts Open()
})
```

If any connected hook returns an error, the remaining connected hooks are
skipped and `Open()` returns that error, mirroring how a before hook aborts an
action.

### Done Hooks

Done hooks run in LIFO order (last added runs first). Use them for teardown:
stopping a progress renderer, releasing resources, or wrapping the final error.
Every done hook always runs (there is no short-circuit), so cleanup is never
skipped. Each hook receives the current error and may transform it.

```go
client.AddHookDone(func(err error) error {
    progress.Stop()
    return err
})
```

### Scope

Unlike before and after hooks, connected and done hooks are **not** inherited
from GlobalClient. Each project Client starts with no-op connected/done hooks,
so register them on the specific project Client whose lifecycle you want to
bracket.

## Execution Order

### Before Hooks (FIFO)

Before hooks run first-in-first-out:

```go
client.AddHookBefore(checkPermissions)  // Runs 1st
client.AddHookBefore(validateConfig)    // Runs 2nd
client.AddHookBefore(acquireLock)       // Runs 3rd
```

Order: checkPermissions -> validateConfig -> acquireLock

If any before hook returns an error, the action is aborted.

### After Hooks (LIFO)

After hooks run last-in-first-out:

```go
client.AddHookAfter(logBasicInfo)      // Runs 3rd (inner)
client.AddHookAfter(wrapWithContext)   // Runs 2nd (middle)
client.AddHookAfter(sendToMonitoring)  // Runs 1st (outer)
```

Order: sendToMonitoring -> wrapWithContext -> logBasicInfo

### Connected (FIFO) and Done (LIFO)

Lifecycle hooks follow the same ordering rules, fired once each:

```go
client.AddHookConnected(openRenderer)  // Open():  runs 1st
client.AddHookConnected(announceStart) // Open():  runs 2nd

client.AddHookDone(flushRenderer)      // Done():  runs 2nd (inner)
client.AddHookDone(stopRenderer)       // Done():  runs 1st (outer)
```

A connected hook that returns an error aborts `Open()`; done hooks always all
run.

## Common Patterns

### Dry Run Mode

```go
dryRun := true

client.AddHookBefore(func(_ context.Context, action Action, r Resource, args Options, err error) error {
    if dryRun && (action == ActionEnsure || action == ActionDelete) {
        log.Printf("Would %s %s %s", action, r.Kind(), r.Name())
        return errors.New("dry run mode")
    }
    return err
})
```

### Progress Tracking

```go
var completed, total int

client.AddHookBefore(func(_ context.Context, action Action, r Resource, args Options, err error) error {
    total++
    return err
})

client.AddHookAfter(func(_ context.Context, action Action, r Resource, args Options, err error) error {
    completed++
    fmt.Printf("Progress: %d/%d\n", completed, total)
    return err
})
```

For live, per-operation progress (image pulls, lifecycle), see
[Progress](/developer/progress) - the after-hook is what marks each line done.

### Conditional Error Suppression

```go
client.AddHookAfter(func(_ context.Context, action Action, r Resource, args Options, err error) error {
    if errors.Is(err, ErrNotFound) && action == ActionDelete {
        // Already deleted, not an error
        return nil
    }
    return err
})
```

## Hook Scope

Hooks registered on GlobalClient are inherited by all projects:

```go
globalClient.AddHookBefore(globalLogger)  // All projects see this
```

Hooks on a project Client only affect that project:

```go
projectA, _ := globalClient.EnsureProject("projectA", true)
projectA.AddHookBefore(projectALogger)  // Only projectA
```

Connected and Done hooks are the exception: they are never inherited from
GlobalClient and must be registered on the project Client directly (see
[Lifecycle Hooks](#lifecycle-hooks)).

## Concurrency

Before and After hooks run inside the WorkerPool: multiple hooks may fire
concurrently for different resources. Any state shared between hook invocations
must be protected:

```go
var mu sync.Mutex
counts := map[string]int{}

client.AddHookAfter(func(_ context.Context, action Action, r Resource, _ Options, err error) error {
    mu.Lock()
    defer mu.Unlock()

    counts[r.Kind()]++
    return err
})
```

Read-only closures that capture only immutable values (string literals, slices
built before registration) are safe without a mutex.

Connected and Done hooks do not run in the WorkerPool. `Open()` and `Done()`
fire them synchronously on the calling goroutine, once each, so they need no
mutex for their own state.

## Best Practices

**Keep hooks simple** - Complex logic should be in separate functions.

**Pass through errors** - If you don't modify the error, return it unchanged:

```go
return err  // Not: return nil
```

**Append, don't replace** - When adding your own error:

```go
return errors.Join(err, myErr)  // Not: return myErr
```

**Check action type** - Not all hooks need to run for all actions:

```go
if action != ActionEnsure {
    return err  // Skip for other actions
}
```
