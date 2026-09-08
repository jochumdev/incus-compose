---
name: issue-plan
description: Plan an issue without implementing code. Load project context first, then create a detailed implementation plan.
---

# Plan an issue

Run `/hello` to load project context (skip if already loaded this session).

If `$ARGUMENTS` is empty, ask for the issue title before continuing.

Enter plan mode, then plan the issue without implementing any code.

The plan must include:

- relevant files and packages to inspect (check existing patterns first)
- expected implementation approach
- tests or `just` commands to verify the change
- risks, unknowns, or questions to resolve before starting
