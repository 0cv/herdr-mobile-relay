---
description: Draft CHANGELOG.md entries for unreleased changes
argument-hint: "[since tag]"
---
Draft entries under `## [Unreleased]` in `CHANGELOG.md` for the changes since ${1:-the latest `v*` tag}.

1. List the commits with `git log --no-merges <tag>..HEAD`.
2. For each commit you might include, read the real change with `git show --stat` and `git show`, not only its message.
3. Skip commits that are only CI, tests, refactors or tooling with no effect for users.
4. Read the whole `## [Unreleased]` section and append to its existing `### Added`, `### Changed`, `### Fixed` or `### Removed` subsections; never duplicate a subsection or an entry.
5. Write like the existing entries: what changed for the user and why, in plain language, with the issue or pull request number when there is one.
6. Never edit released version sections.

Show the drafted entries and wait for confirmation before committing.
