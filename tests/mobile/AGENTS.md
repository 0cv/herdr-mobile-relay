# Mobile device lane

`.github/workflows/mobile-ci.yml` installs a published release on an Android emulator (pinned Chrome) and an iOS simulator (Home Screen web app), upgrades it to the build under test and runs the release scenario on both platforms in parallel. `check.yml` calls it for pushes to `dev` and `main` and for pull requests into `dev` and `main`. A new push to a pull request cancels that pull request's previous run. Every push to a branch gets its own run and is never cancelled.

## Release baselines

- `baselines.json` lists the latest release first and the release before it. The lane upgrades from the first listed release whose web bundle differs from the build under test.
- After a release, add it first and drop the oldest entry. `archiveSha256` comes from the release's `checksums.txt`. `revision` and `webHash` (`web_hash`) come from `release-manifest.json` in the `linux_amd64` archive. `assets`, `entry`, `build`, `script` and `style` come from `web/version.json` in the same archive. The prepare job checks every value against the downloaded archive.

## Rules

- Never weaken a product assertion to get a green run. Limits such as at most 2 reloads and at most 2 lifecycle launches describe what users must experience.
- `APPIUM_TIMEOUT` stays fatal for the rest of the run because the driver can no longer be trusted. Do not retry inside the run.
- Hosted iOS runs with `MOBILE_TIMING_SCALE=3`, which scales phase and request allowances but not admission. The release scenario has 45 minutes and the device job 60.
- Appium's iOS remote debugger deletes properties named `ceil`, `clone`, `floor`, `round`, `scale` and `toString` from web script results. Do not use those names in returned objects.
- The Android environment check fails on any process death it cannot explain. When a hosted run fails with `ADVERSE_DEATH`:
  1. Commit a trimmed copy of the hosted logcat to `unit/fixtures/`.
  2. Add a test that accepts the new case and still rejects crashes, signals, low-memory kills, force stops, package changes and PID reuse.
  3. Replay earlier recorded runs before widening what is accepted.
- Raw logs, screenshots and page XML stay in private artifacts with one-day retention. Public summaries carry only the bounded fields built by `publicMobileSummary` in `support/android-product.ts`. Never print raw device output in step logs.
- Treat a failed download, runner image or simulator boot as an infrastructure fault: rerun the whole workflow, never single jobs. A lane change is done after two green attempts on the same commit.

## Tests

- Run tests with `MOBILE_UNIT_FILTER='<regex>' bun run --cwd tests/mobile test:unit`. The slow groups are:
  - `^iOS `: about 18 minutes. Narrow it with `IOS_TEST_FILTER`.
  - `^Android product`: about 11 minutes.
  - `^Android production CLI `: about 11 minutes.

  Run the group you changed first, then all groups before pushing.
- To show that a new test catches the bug, run it against the previous version of the source file and confirm that it fails.
