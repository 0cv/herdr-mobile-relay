<script lang="ts">

  import Button from '$components/ui/Button.svelte';
  import { relayStore } from '$lib/store';
  import type { Agent, WorkspaceFile, WorkspaceGitDiff, WorkspaceGitStatus, WorkspaceTree } from '$lib/types';

  let {
    open = $bindable(false),
    agent,
  }: {
    open?: boolean;
    agent: Agent | null;
  } = $props();

  let dialog = $state<HTMLDialogElement>();
  let query = $state('');
  let section = $state<'files' | 'changes'>('files');
  let tree = $state<WorkspaceTree | null>(null);
  let git = $state<WorkspaceGitStatus | null>(null);
  let preview = $state<WorkspaceFile | WorkspaceGitDiff | null>(null);
  let previewKind = $state<'file' | 'diff'>('file');
  let selectedPath = $state('');
  let loading = $state(false);
  let previewLoading = $state(false);
  let error = $state('');
  let gitReason = $state('');
  let workspaceGeneration = 0;
  let previewGeneration = 0;
  let loadedIdentity = '';
  let sidebarOpen = $state(true);
  let sidebarSwipe: { pointerId: number; x: number; y: number } | null = null;
  const MIN_DIFF_ZOOM = 0.7;
  const MAX_DIFF_ZOOM = 2.5;
  const DIFF_ZOOM_STEP = 0.15;
  let diffZoom = $state(1);
  const diffPointers = new Map<number, { x: number; y: number }>();
  let diffPinch: { distance: number; zoom: number } | null = null;

  const filteredEntries = $derived((tree?.entries || []).filter((entry) => {
    const needle = query.trim().toLocaleLowerCase();
    return !needle || entry.path.toLocaleLowerCase().includes(needle);
  }));
  const filteredChanges = $derived((git?.files || []).filter((entry) => {
    const needle = query.trim().toLocaleLowerCase();
    return !needle || `${entry.path} ${entry.original_path || ''}`.toLocaleLowerCase().includes(needle);
  }));

  $effect(() => {
    if (open && dialog && !dialog.open) dialog.showModal();
  });

  $effect(() => {
    const target = agent;
    const identity = target ? `${target.pane_id}\u0000${String(target.cwd || '')}` : '';
    if (!open || !target) {
      loadedIdentity = '';
      return;
    }
    if (identity === loadedIdentity) return;
    loadedIdentity = identity;
    void loadWorkspace(target);
  });

  function close() {
    workspaceGeneration += 1;
    previewGeneration += 1;
    loadedIdentity = '';
    loading = false;
    previewLoading = false;
    open = false;
  }

  function cancel(event: Event) {
    event.preventDefault();
    close();
  }

  function startSidebarSwipe(event: PointerEvent) {
    if (event.pointerType !== 'touch' || !event.isPrimary) return;
    sidebarSwipe = { pointerId: event.pointerId, x: event.clientX, y: event.clientY };
    const target = event.currentTarget;
    if (target instanceof HTMLElement) {
      try {
        target.setPointerCapture(event.pointerId);
      } catch {
        // Synthetic events and older webviews may not register an active pointer.
      }
    }
  }

  function finishSidebarSwipe(event: PointerEvent) {
    if (!sidebarSwipe || sidebarSwipe.pointerId !== event.pointerId) return;
    const start = sidebarSwipe;
    sidebarSwipe = null;
    const target = event.currentTarget;
    if (target instanceof HTMLElement && target.hasPointerCapture(event.pointerId)) {
      try {
        target.releasePointerCapture(event.pointerId);
      } catch {
        // The pointer can be released by the browser before this handler runs.
      }
    }
    const horizontal = start.x - event.clientX;
    const vertical = Math.abs(start.y - event.clientY);
    if (horizontal < 48 || horizontal < vertical * 1.25) return;
    event.preventDefault();
    sidebarOpen = false;
  }

  function cancelSidebarSwipe() {
    sidebarSwipe = null;
  }
  function setDiffZoom(value: number) {
    diffZoom = Math.min(MAX_DIFF_ZOOM, Math.max(MIN_DIFF_ZOOM, value));
  }

  function resetDiffZoom() {
    diffZoom = 1;
    diffPointers.clear();
    diffPinch = null;
  }

  function diffPointerDistance(): number {
    const points = [...diffPointers.values()];
    if (points.length !== 2) return 0;
    return Math.hypot(points[0].x - points[1].x, points[0].y - points[1].y);
  }

  function startDiffPinch(event: PointerEvent) {
    if (event.pointerType !== 'touch' || diffPointers.size >= 2) return;
    diffPointers.set(event.pointerId, { x: event.clientX, y: event.clientY });
    const target = event.currentTarget;
    if (target instanceof HTMLElement) {
      try {
        target.setPointerCapture(event.pointerId);
      } catch {
        // Synthetic events and older webviews may not register an active pointer.
      }
    }
    const distance = diffPointerDistance();
    if (distance > 0) diffPinch = { distance, zoom: diffZoom };
  }

  function moveDiffPinch(event: PointerEvent) {
    if (!diffPointers.has(event.pointerId)) return;
    diffPointers.set(event.pointerId, { x: event.clientX, y: event.clientY });
    if (!diffPinch || diffPointers.size !== 2) return;
    const distance = diffPointerDistance();
    if (distance <= 0) return;
    event.preventDefault();
    setDiffZoom(diffPinch.zoom * distance / diffPinch.distance);
  }

  function finishDiffPinch(event: PointerEvent) {
    if (!diffPointers.delete(event.pointerId)) return;
    const target = event.currentTarget;
    if (target instanceof HTMLElement && target.hasPointerCapture(event.pointerId)) {
      try {
        target.releasePointerCapture(event.pointerId);
      } catch {
        // The browser may release capture before pointerup reaches this handler.
      }
    }
    diffPinch = null;
  }

  function zoomDiffWheel(event: WheelEvent) {
    if (!event.ctrlKey) return;
    event.preventDefault();
    setDiffZoom(diffZoom * Math.exp(-event.deltaY * 0.01));
  }

  function diffLineTone(line: string): string {
    if (line.startsWith('diff --git ') || line.startsWith('index ') || line.startsWith('new file mode ')
      || line.startsWith('deleted file mode ') || line.startsWith('similarity index ')
      || line.startsWith('rename from ') || line.startsWith('rename to ')
      || line.startsWith('Binary files ')) return 'diff-meta';
    if (line.startsWith('--- ') || line.startsWith('+++ ')) return 'diff-file';
    if (line.startsWith('@@')) return 'diff-hunk';
    if (line.startsWith('+')) return 'diff-addition';
    if (line.startsWith('-')) return 'diff-deletion';
    if (line.startsWith('\\ ')) return 'diff-note';
    return 'diff-context';
  }


  async function loadWorkspace(target: Agent) {
    const current = ++workspaceGeneration;
    previewGeneration += 1;
    query = '';
    sidebarOpen = true;
    section = 'files';
    tree = null;
    git = null;
    preview = null;
    previewLoading = false;
    selectedPath = '';
    resetDiffZoom();
    error = '';
    gitReason = '';
    loading = true;
    try {
      const [treeResult, gitResult] = await Promise.allSettled([
        relayStore.loadWorkspaceTree(target),
        relayStore.loadWorkspaceGitStatus(target),
      ]);
      if (current !== workspaceGeneration) return;
      if (treeResult.status === 'rejected') throw treeResult.reason;
      tree = treeResult.value;
      if (gitResult.status === 'fulfilled') {
        git = gitResult.value;
        if (!git.available) gitReason = 'This workspace is not inside a Git repository.';
      } else {
        git = { available: false, files: [] };
        gitReason = gitResult.reason instanceof Error
          ? gitResult.reason.message
          : 'Git status could not be read.';
      }
    } catch (loadError) {
      if (current === workspaceGeneration) error = (loadError as Error).message;
    } finally {
      if (current === workspaceGeneration) loading = false;
    }
  }

  async function showFile(path: string) {
    if (!agent) return;
    const current = ++previewGeneration;
    selectedPath = path;
    preview = null;
    previewKind = 'file';
    resetDiffZoom();
    error = '';
    previewLoading = true;
    try {
      const next = await relayStore.loadWorkspaceFile(agent, path);
      if (current === previewGeneration) preview = next;
    } catch (loadError) {
      if (current === previewGeneration) error = (loadError as Error).message;
    } finally {
      if (current === previewGeneration) previewLoading = false;
    }
  }

  async function showDiff(path: string) {
    if (!agent) return;
    const current = ++previewGeneration;
    selectedPath = path;
    preview = null;
    previewKind = 'diff';
    resetDiffZoom();
    error = '';
    previewLoading = true;
    try {
      const next = await relayStore.loadWorkspaceGitDiff(agent, path);
      if (current === previewGeneration) preview = next;
    } catch (loadError) {
      if (current === previewGeneration) error = (loadError as Error).message;
    } finally {
      if (current === previewGeneration) previewLoading = false;
    }
  }

  function selectSection(next: 'files' | 'changes') {
    if (section === next) return;
    previewGeneration += 1;
    previewLoading = false;
    section = next;
    sidebarOpen = true;
    preview = null;
    selectedPath = '';
    resetDiffZoom();
    error = '';
  }

  function statusLabel(status: string): string {
    if (status === '??') return 'New';
    if (status.includes('D')) return 'Deleted';
    if (status.includes('R')) return 'Renamed';
    if (status.includes('A')) return 'Added';
    if (status.includes('M')) return 'Modified';
    return status.trim() || 'Changed';
  }

  function pathDepth(path: string): number {
    return Math.min(8, Math.max(0, path.split('/').length - 1));
  }
</script>

{#if open}
  <dialog bind:this={dialog} class="workspace-dialog" aria-labelledby="workspace-dialog-title" oncancel={cancel} onclose={close}>
    <div class="workspace-inspector">
      <header>
        <div>
          <h2 id="workspace-dialog-title">Workspace</h2>
          <p>{String(agent?.cwd || 'Workspace unavailable')}</p>
        </div>
        <Button variant="ghost" size="icon" class="workspace-close" aria-label="Close workspace inspector" onclick={close}>×</Button>
      </header>

      <div class="workspace-toolbar">
        <div class="workspace-section-tabs" role="tablist" aria-label="Workspace sections">
          <button class:active={section === 'files'} type="button" role="tab" aria-selected={section === 'files'} onclick={() => selectSection('files')}>Files</button>
          <button class:active={section === 'changes'} type="button" role="tab" aria-selected={section === 'changes'} onclick={() => selectSection('changes')}>Changes{#if git?.files.length} ({git.files.length}){/if}</button>
        </div>
        <Button
          class="workspace-sidebar-toggle"
          variant="ghost"
          size="icon"
          aria-label={sidebarOpen ? `Hide ${section === 'files' ? 'file list' : 'changed-file list'}` : `Show ${section === 'files' ? 'file list' : 'changed-file list'}`}
          title={sidebarOpen ? 'Hide sidebar' : 'Show sidebar'}
          onclick={() => { sidebarOpen = !sidebarOpen; }}
        >
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true" focusable="false">
            <path d="M4 6h16M4 12h16M4 18h16"></path>
          </svg>
        </Button>
        {#if git?.available && git.branch}<span class="git-branch" title="Git branch">{git.branch}</span>{/if}
        {#if git?.available}
          {#if git.ahead !== undefined && git.behind !== undefined}
            <span class="git-branch" title="Commits relative to the configured upstream">↑{git.ahead} ↓{git.behind}</span>
          {:else}
            <span class="git-branch" title="This branch has no configured upstream">No upstream</span>
          {/if}
        {/if}
        <label class="workspace-search">
          <span class="sr-only">Filter workspace {section}</span>
          <input bind:value={query} type="search" placeholder={`Filter ${section}…`} autocomplete="off" />
        </label>
      </div>

      <div class:sidebar-collapsed={!sidebarOpen} class="workspace-body">
        {#if sidebarOpen}<aside
          aria-label={section === 'files' ? 'Workspace files' : 'Changed files'}
          onpointerdown={startSidebarSwipe}
          onpointerup={finishSidebarSwipe}
          onpointercancel={cancelSidebarSwipe}
        >
          {#if loading}
            <p class="workspace-message" role="status">Reading workspace…</p>
          {:else if section === 'files'}
            {#if tree?.truncated}<p class="workspace-notice">File list limited to the first 4,000 entries.</p>{/if}
            {#each filteredEntries as entry (entry.path)}
              {#if entry.kind === 'directory'}
                <div class="workspace-directory" style={`--path-depth: ${pathDepth(entry.path)}`} title={entry.path}>▸ {entry.name}</div>
              {:else}
                <button class:active={selectedPath === entry.path && previewKind === 'file'} style={`--path-depth: ${pathDepth(entry.path)}`} type="button" title={entry.path} onclick={() => showFile(entry.path)}>{entry.name}</button>
              {/if}
            {:else}
              <p class="workspace-message">No matching files.</p>
            {/each}
          {:else if git?.available}
            {#if git.truncated}<p class="workspace-notice">Changed-file list is truncated.</p>{/if}
            {#each filteredChanges as changed (changed.path)}
              <button class:active={selectedPath === changed.path && previewKind === 'diff'} type="button" title={changed.path} onclick={() => showDiff(changed.path)}>
                <span>{changed.path}</span><em>{statusLabel(changed.status)}</em>
              </button>
            {:else}
              <p class="workspace-message">No matching changes.</p>
            {/each}
          {:else}
            <p class="workspace-message">{gitReason || 'Git status is unavailable for this workspace.'}</p>
          {/if}
        </aside>{/if}

        <main aria-label="Workspace preview">
          {#if previewLoading}
            <p class="workspace-message" role="status">Loading {selectedPath}…</p>
          {:else if error}
            <p class="workspace-error" role="alert">{error}</p>
          {:else if preview && 'kind' in preview && preview.kind === 'image' && preview.data_url}
            <figure>
              <img src={preview.data_url} alt={`Preview of ${preview.path}`} />
              <figcaption>{preview.path} · {Math.ceil(preview.size / 1024)} KB</figcaption>
            </figure>
          {:else if preview && 'kind' in preview}
            <header class="preview-heading"><strong>{preview.path}</strong><span>{Math.ceil(preview.size / 1024)} KB</span></header>
            <textarea class="workspace-code-preview" readonly wrap="off" spellcheck="false" aria-label={`Contents of ${preview.path}`} value={preview.text || ''}></textarea>
          {:else if preview && 'diff' in preview}
            <header class="preview-heading">
              <strong>{preview.path}</strong>
              <div class="preview-heading-meta">
                <span>Unified diff</span>
                <div class="workspace-diff-zoom" role="group" aria-label="Diff zoom">
                  <button type="button" aria-label="Zoom out diff" title="Zoom out" onclick={() => setDiffZoom(diffZoom - DIFF_ZOOM_STEP)}>−</button>
                  <button type="button" aria-label="Reset diff zoom" title="Reset zoom" onclick={resetDiffZoom}>{Math.round(diffZoom * 100)}%</button>
                  <button type="button" aria-label="Zoom in diff" title="Zoom in" onclick={() => setDiffZoom(diffZoom + DIFF_ZOOM_STEP)}>+</button>
                </div>
              </div>
            </header>
            {#if preview.diff}
              <pre
                class="workspace-code-preview workspace-diff"
                style={`--diff-zoom: ${diffZoom}`}
                aria-label={`Diff for ${preview.path}`}
                onpointerdown={startDiffPinch}
                onpointermove={moveDiffPinch}
                onpointerup={finishDiffPinch}
                onpointercancel={finishDiffPinch}
                onwheel={zoomDiffWheel}
              >{#each preview.diff.split('\n') as line, index (index)}<span class={`workspace-diff-line ${diffLineTone(line)}`}>{line}</span>{/each}</pre>
            {:else}
              <p class="workspace-message">No text diff for this file.</p>
            {/if}
          {:else}
            <p class="workspace-message">Choose a {section === 'files' ? 'file' : 'change'} to preview it.</p>
          {/if}
        </main>
      </div>
    </div>
  </dialog>
{/if}
