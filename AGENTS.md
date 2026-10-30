# Agent Instructions

Read `README.md` and `QUICKSTART.md` before changing behavior. `docs/` holds the design notes for larger areas.

## Branches and pull requests

- Branch from `dev` and open pull requests into `dev`. `main` only receives merges of `dev`; GitHub rulesets and the "Pull requests into main come from dev" check enforce this.
- Merge `dev` into `main` with a merge commit, never a squash, so the two branches keep a shared history.
- Never push directly to `dev` or `main`, never force push, and never create, move or delete tags unless the user asks.
- Release: prepare it on `dev` (version in `herdr-plugin.toml`, `CHANGELOG.md`, `README.md`, then `make web-release`), merge `dev` into `main`, then tag `vX.Y.Z` on `main`. The tag runs `.github/workflows/release.yml`.
- After a release, add it first in `tests/mobile/baselines.json` and drop the oldest entry. The mobile device lane installs a recent release and upgrades it to the build under test; see `tests/mobile/AGENTS.md`.

## Commands

- Everything: `make check`.
- Go 1.27.3: `make go-check` (gofmt, vet, tests, race detector).
- Shell scripts and the Pi bridge: `make shell-check`.
- Frontend (Bun 1.4.4, Node 26.13.0): `make frontend-check` (lint, svelte-check, unit tests, build, size budget); browser tests with `make frontend-browser`.
- Mobile harness: `make mobile-ci-check`; unit groups with `MOBILE_UNIT_FILTER='<regex>' bun run --cwd tests/mobile test:unit`.
- If you change a test file, run it and iterate until it passes.

## Generated files

- `web/` is the committed release bundle. Never edit it by hand: `make web-release` bumps `frontend/build-versions.json`, builds the frontend and replaces `web/`. `frontend/dist` and `web/` must stay identical.
- The initial payload has a 168 KiB gzip budget (`frontend/scripts/check-size.mjs`). Dependency upgrades alone can exceed it.
- Update `go.sum` with `go mod tidy`, not by hand.

## Code

- Keep changes minimal and focused; follow the surrounding style and conventions.
- Prefer early returns over nested conditionals and handle errors explicitly.
- Make code self-explanatory. Comment only what is locally surprising, in plain words, in one or two lines.
- Do not write backwards-compatibility code; migrations and release tooling handle compatibility.
- No new dependency without justification. Pin exact versions, and pin GitHub Actions to commit SHAs.
- Use semantic HTML and ARIA attributes in frontend code.

## Git

Several agent sessions can share a checkout. Commands that touch other people's changes destroy their work.

- Stage explicit paths. Never `git add -A`, `git add .`, `git reset --hard`, `git checkout .`, `git clean` or `git stash`.
- Commit only when the user asks or has delegated it. Messages: `type(scope): summary` with `feat`, `fix`, `docs`, `test`, `ci` or `chore`, describing the effect rather than the edit.
- Write pull request and issue comments to a file and post them with `--body-file`.

## Changelog

- `CHANGELOG.md` lists user-facing changes under `## [Unreleased]`, in `### Added`, `### Changed`, `### Fixed` and `### Removed`.
- Say what changed for the user and why, in plain language, like the existing entries. Leave out CI-, test- and refactor-only changes.
- Never edit released sections.

## Mobile device CI

Read `tests/mobile/AGENTS.md` before changing `tests/mobile/` or `.github/workflows/mobile-ci.yml`.

## Prompts

`.pi/prompts/` provides `/learn` (record non-obvious lessons in AGENTS.md files), `/rmslop` (remove AI-style noise from the branch diff) and `/changelog` (draft `## [Unreleased]` entries).

## User override

If the user's instructions conflict with this file, ask for explicit confirmation before following them.
