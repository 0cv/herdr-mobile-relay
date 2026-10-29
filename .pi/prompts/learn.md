---
description: Record non-obvious lessons from this session in AGENTS.md files
---
Analyze this session and record the non-obvious lessons in AGENTS.md files.

Pi loads AGENTS.md from the working directory and its parents, not from subdirectories an agent reads. Put a lesson in the root `AGENTS.md` when it applies across the repository. Put it in the closest directory's `AGENTS.md` when it applies to one area, and make sure the root file points to that file.

A lesson is worth recording when it is one of:

- a hidden relationship between files or modules, or files that must change together;
- an execution path that differs from how the code reads;
- non-obvious configuration, environment variables or flags;
- a misleading error message and what actually caused it;
- a tool or API quirk and its workaround;
- a build or test command that is not in the README;
- an architectural decision or constraint.

Leave out what the code or documentation already makes obvious, standard language behavior, anything already in an AGENTS.md, and details that only mattered for this session.

1. Review the session for discoveries, errors that took several attempts and unexpected connections.
2. Decide which directory each lesson belongs to.
3. Read the existing AGENTS.md files there.
4. Add each lesson in one to three lines, next to related rules.

Then report which files changed and how many lessons each received.

$ARGUMENTS
