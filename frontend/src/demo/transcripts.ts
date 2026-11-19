import { rolloutCheck, searchCheck } from './checks';
import type { demoProfiles } from './scene';

export const transcripts: Record<string, string> = {
  'atlas-review-terminal': `Codex · Patch review
Sample session · search rollout

› Review the pagination patch.
  Check empty results and keyboard focus.

• Plan
  1. Read the patch and existing tests.
  2. Trace focus after changing the query.
  3. Decide what to run before merging.

• Explored
  └ Read src/search/Results.svelte
  └ Read tests/search.test.ts
  └ Read docs/release-notes.md

• Reviewed the search pagination patch.
  Reset clears the previous query.
  The empty page returns focus to search.
  It also keeps the classic search link.

• Checked fixture expectations
  └ Page resets when the query changes.
  └ Clear puts the caret back in the box.
  └ No matches suggest a shorter query.

• Review summary
  No unexpected changes in this checkout.
  The test covers the new keyboard path.
  One verification step remains: make test.

Run make test before wrapping up? Waiting for approval.`,
  'atlas-plan-terminal': `Claude Code · Release plan
Sample session · search rollout

> Plan the rollout for the new search page.

● Read(docs/release-notes.md)
  ⎿  Read the current draft and checklist
● Read(src/search/Results.svelte)
  ⎿  Found the new empty-state controls
● Read(docs/preview-guide.md)
  ⎿  Found the classic search fallback

● The search page is ready for a rollout decision.
  Two paths use the same build:

  1. Preview first
     Enable the flag for a small group.
     Collect feedback on focus and results.
     Widen access after the review.

  2. All at once
     Include search in the next release.
     Watch feedback from all workspaces.
     Keep the classic page available.

● Proposed checklist
  ☐ Choose when to enable the flag
  ☐ Verify no-match and clear behavior
  ☐ Confirm the classic search fallback
  ☐ Record the decision in the notes

How should we roll out the new search page?`,
  'atlas-notes-terminal': `Pi · Release notes
Sample session · search rollout

> Review the search rollout plan
Read docs/release-notes.md
  Draft assumes a general rollout.
  Classic search is still available.
Plan: document a small preview first.

> Draft a note for the search preview
Edit docs/release-notes.md
  Replaced the general rollout line.
  Added a short keyboard checklist.
  Added the fallback instructions.

Preview URL
  /search?preview=1
  Enabled only for the preview group.

> Verify the rollout before review
$ ${rolloutCheck.command}
  loading search rollout fixtures
  ${rolloutCheck.cases.map((name) => `pass  ${name}`).join('\n  ')}
  ${rolloutCheck.cases.length} passed in the sample log

Summary
  Drafted the search preview note.
  Documented how to disable the flag.
  Left rollout timing with the reviewer.

Ready for the next task.`,
  'atlas-search-terminal': `OpenCode · Search polish
Sample session · search rollout

> Check the search page before preview.

# Task: verify the keyboard path
  [x] Read results and search fixtures
  [x] Review the empty-state controls
  [x] Check focus after clearing a query
  [ ] Summarize the preview checklist

→ Read src/search/Results.svelte
  Found an empty state with a Clear button.
  Reset sends focus back to the search box.
→ Read tests/search.test.ts
  Found keyboard and pagination fixtures.

# Bash: check the search fixtures
$ ${searchCheck.command}
  loading search preview fixtures…
  ${searchCheck.result.replaceAll('\n', '\n  ')}

# Review: empty and cleared queries
  Empty query: show a search hint.
  No matches: keep Clear within reach.
  Clear: reset the page and restore focus.

→ Read docs/release-notes.md
  Preview checklist matches the UI.
Checking keyboard focus and the empty search state…`,
  'atlas-fixtures-terminal': `Pi · Fixture checks
Sample session · search rollout

> Check the fixtures in fix/one
Read tests/search.test.ts
  Uses a small set of made-up entries.
  The empty and populated cases share setup.
Read src/search/Results.svelte
  Clear resets both query and focus.

$ git status --short
   M src/search/Results.svelte
   M docs/release-notes.md

$ ${searchCheck.command}
  loading keyboard and results fixtures
  ${searchCheck.result.replaceAll('\n', '\n  ')}

$ make lint
  checked results and test fixture
  no warnings in the sample log

Summary
  No new edits needed in fix/one.
  Tests cover the empty page and reset.
  The preview notes match the fixtures.

Fixture review complete.`,
  'birch-docs-terminal': `Claude Code · Docs preview
Sample session · search rollout

> Check the search preview guide.

● Loaded sample guides in docs/.
● Read(docs/preview-guide.md)
  ⎿  Found both search links
● Read(docs/release-notes.md)
  ⎿  Read the rollout checklist
● Read(docs/keyboard.md)
  ⎿  Checked search shortcuts

● Guide notes
  Try search links to the preview.
  Classic stays in that section.
  The empty-state hint matches.

● Bash(make docs)
  ⎿  Reading guide pages
     building overview
     building search preview
     building keyboard shortcuts
     checking internal links
     3 pages built in sample log

● Review
  ✓ Preview and fallback links
  ✓ Clear search is documented
  ✓ Examples use made-up entries
  · Checking navigation order

Building the sample docs preview…`,
  'birch-examples-terminal': `Codex · Example index
Sample session · search rollout

› Review the guide’s examples.

• Plan
  1. Read the example index.
  2. Check matches and misses.
  3. Check the guide labels.

• Explored
  └ Read search/index.md
  └ Read search/queries.txt
  └ Read docs/preview-guide.md

• Reviewed examples
  "paper lantern" has two results.
  "copper bowl" has one result.
  "quiet planet" has no matches.
  Clear returns to page one.

• Checked sample index
  └ make examples
    3 example queries checked
    0 missing labels (sample)

• Summary
  Examples cover both outcomes.
  Labels agree with the guide.
  The index is ready for review.

No changes requested.`,
};

type DemoProfile = typeof demoProfiles[number];
const profileTranscript = {
  codex: 'birch-examples-terminal',
  claude: 'birch-docs-terminal',
  pi: 'atlas-notes-terminal',
  opencode: 'atlas-search-terminal',
} satisfies Record<DemoProfile['id'], string>;

export function launchTranscript(profile: DemoProfile, name: string, cwd: string): string {
  const body = transcripts[profileTranscript[profile.id]].split('\n').slice(1, -1);
  return [`${profile.label} · ${name}`, ...body, `Sample agent ready in ${cwd}. No process was started.`].join('\n');
}
