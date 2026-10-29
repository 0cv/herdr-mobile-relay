---
description: Remove AI-style noise from this branch's changes
---
Compare this branch with `origin/dev` (`git diff origin/dev...HEAD` plus uncommitted changes) and remove AI-style noise introduced on this branch:

- comments a maintainer would not write, or that differ from the rest of the file;
- defensive checks, fallbacks or try/catch blocks that are unusual for that area, especially on trusted or already validated paths;
- casts to `any` or similar escapes around type errors;
- compatibility shims, unused options and speculative abstractions;
- emoji, filler wording, and any other style that differs from the file.

Change only code this branch introduced. Then run the checks for the touched area and report in one to three sentences what you removed.
