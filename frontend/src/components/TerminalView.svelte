<script lang="ts">
  import { onMount, tick, untrack } from 'svelte';
  import Button from '$components/ui/Button.svelte';
  import QuestionForm from '$components/QuestionForm.svelte';
  import {
    agentStatusGroup,
    approvalButtonTone,
    approvalOptions,
    questionInteraction,
    sortedAgents,
  } from '$lib/agents';
  import { showAgentStatusLine } from '$lib/preferences';
  import { replaceView } from '$lib/router';
  import { relayStore } from '$lib/store';
  import { lastCompletedResponse, renderTerminalContent } from '$lib/terminal';
  import type { Agent, SlashCommand, SlashCommandCatalog, TerminalFrame } from '$lib/types';

  let {
    agent,
    allAgents,
    frame,
    responding,
  }: {
    agent: Agent;
    allAgents: Agent[];
    frame?: TerminalFrame;
    responding: Set<string>;
  } = $props();

  let terminalElement = $state<HTMLDivElement>(null!);
  let fileInput = $state<HTMLInputElement>(null!);
  let composerElement = $state<HTMLTextAreaElement>(null!);
  let composer = $state('');
  let composerFocused = $state(false);
  let deferredFrame: TerminalFrame | undefined;
  let displayed = $state('');
  let renderedHtml = $state('');
  let lastFormat = '';
  let jumpVisible = $state(false);
  let arrowsOpen = $state(false);
  let uploadStatus = $state('');
  let uploadError = $state(false);
  let requestedPaneId = '';
  let slashCatalog = $state<SlashCommandCatalog>({ commands: [], truncated: false });
  let slashCatalogLoading = $state(true);
  let slashCatalogUnavailable = $state(false);
  let activeSlashIndex = $state(0);
  let dismissedSlashQuery = $state<string | null>(null);

  const blocked = $derived(agentStatusGroup(agent) === 'blocked');
  const interaction = $derived(questionInteraction(agent));
  const questionMode = $derived(Boolean(blocked && interaction));
  const options = $derived(approvalOptions(agent));
  const nextBlocked = $derived(sortedAgents(allAgents.filter((item) => agentStatusGroup(item) === 'blocked' && item.pane_id !== agent.pane_id))[0]);
  const slashQuery = $derived(composer.startsWith('/') && !/\s/.test(composer) ? composer.slice(1).toLocaleLowerCase() : null);
  const filteredSlashCommands = $derived.by(() => {
    if (slashQuery === null) return [];
    if (!slashQuery) return slashCatalog.commands;
    return slashCatalog.commands.filter((entry) => entry.command.slice(1).toLocaleLowerCase().startsWith(slashQuery));
  });
  const effectiveSlashIndex = $derived(filteredSlashCommands.length
    ? Math.min(activeSlashIndex, filteredSlashCommands.length - 1)
    : -1);
  const slashMenuOpen = $derived(!blocked
    && !questionMode
    && slashQuery !== null
    && dismissedSlashQuery !== composer);

  $effect(() => {
    const next = frame;
    const statusLine = $showAgentStatusLine;
    if (!next || next.paneId !== agent.pane_id) {
      displayed = 'Loading…';
      renderedHtml = 'Loading…';
      lastFormat = '';
      deferredFrame = undefined;
      jumpVisible = false;
      return;
    }
    if (untrack(() => composerFocused)) deferredFrame = next;
    else void applyFrame(next, statusLine);
  });

  $effect(() => {
    const paneId = agent.pane_id;
    if (paneId === requestedPaneId) return;
    requestedPaneId = paneId;
    relayStore.readPane(agent);
  });

  $effect(() => {
    if (!slashMenuOpen || effectiveSlashIndex < 0) return;
    const optionId = `slash-command-option-${effectiveSlashIndex}`;
    void tick().then(() => document.getElementById(optionId)?.scrollIntoView?.({ block: 'nearest' }));
  });

  onMount(() => {
    let mounted = true;
    void relayStore.loadSlashCommands(agent).then((catalog) => {
      if (!mounted) return;
      slashCatalog = catalog;
      slashCatalogUnavailable = false;
    }).catch(() => {
      if (mounted) slashCatalogUnavailable = true;
    }).finally(() => {
      if (mounted) slashCatalogLoading = false;
    });
    const refresh = setInterval(() => relayStore.readPane(agent), 3_000);
    return () => {
      mounted = false;
      clearInterval(refresh);
    };
  });

  async function applyFrame(next: TerminalFrame, statusLine = $showAgentStatusLine) {
    const rendered = renderTerminalContent(next.content, next.format, String(agent.agent || ''), statusLine);
    if (rendered.display === displayed && next.format === lastFormat) return;
    const distance = terminalElement
      ? terminalElement.scrollHeight - terminalElement.scrollTop - terminalElement.clientHeight
      : 0;
    const stick = distance < 48;
    const previousTop = terminalElement?.scrollTop || 0;
    displayed = rendered.display;
    renderedHtml = rendered.html;
    lastFormat = next.format;
    await tick();
    if (!terminalElement) return;
    if (stick) {
      terminalElement.scrollTop = terminalElement.scrollHeight;
      jumpVisible = false;
    } else {
      terminalElement.scrollTop = previousTop;
      jumpVisible = true;
    }
  }

  function focusComposer(event: FocusEvent) {
    const target = event.target;
    if (!(target instanceof HTMLTextAreaElement)
      && !(target instanceof HTMLInputElement && target.classList.contains('question-other-input'))) return;
    composerFocused = true;
  }

  function blurComposer() {
    setTimeout(() => {
      const active = document.activeElement;
      if (active instanceof HTMLTextAreaElement
        || (active instanceof HTMLInputElement && active.classList.contains('question-other-input'))) return;
      composerFocused = false;
      const pending = deferredFrame;
      deferredFrame = undefined;
      if (pending) void applyFrame(pending);
    });
  }

  async function sendPrompt() {
    const text = composer.replace(/[\r\n]+$/g, '');
    if (!text || blocked) return;
    composer = '';
    try {
      await relayStore.sendToAgent(agent, { type: 'submit_prompt', text });
      relayStore.showToast('Prompt sent.');
    } catch (error) {
      if (!composer) composer = text;
      relayStore.showToast((error as Error).message, true);
    }
    setTimeout(() => relayStore.readPane(agent), 500);
  }

  function composerInput() {
    if (dismissedSlashQuery !== composer) dismissedSlashQuery = null;
    activeSlashIndex = 0;
  }

  async function selectSlashCommand(command: SlashCommand) {
    composer = `${command.command}${command.argument_hint ? ' ' : ''}`;
    dismissedSlashQuery = composer;
    activeSlashIndex = 0;
    await tick();
    composerElement.focus();
    composerElement.setSelectionRange(composer.length, composer.length);
  }

  function keydown(event: KeyboardEvent) {
    if (event.isComposing) return;
    if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      void sendPrompt();
      return;
    }
    if (!slashMenuOpen) return;
    if (event.key === 'Escape') {
      event.preventDefault();
      dismissedSlashQuery = composer;
      return;
    }
    if (event.key === 'ArrowDown' && filteredSlashCommands.length) {
      event.preventDefault();
      activeSlashIndex = effectiveSlashIndex >= filteredSlashCommands.length - 1 ? 0 : effectiveSlashIndex + 1;
      return;
    }
    if (event.key === 'ArrowUp' && filteredSlashCommands.length) {
      event.preventDefault();
      activeSlashIndex = effectiveSlashIndex <= 0 ? filteredSlashCommands.length - 1 : effectiveSlashIndex - 1;
      return;
    }
    if ((event.key === 'Enter' || event.key === 'Tab') && effectiveSlashIndex >= 0) {
      event.preventDefault();
      void selectSlashCommand(filteredSlashCommands[effectiveSlashIndex]);
    }
  }

  async function sendKeys(keys: string[], activityLabel = '') {
    try {
      await relayStore.sendToAgent(agent, { type: 'send_keys', keys, activity_label: activityLabel });
    } catch (error) {
      relayStore.showToast((error as Error).message, true);
    }
    setTimeout(() => relayStore.readPane(agent), 300);
  }

  async function copyResponse() {
    const response = lastCompletedResponse(displayed);
    if (!response) {
      relayStore.showToast('No completed response is visible yet.', true);
      return;
    }
    try {
      if (navigator.clipboard?.writeText) await navigator.clipboard.writeText(response);
      else {
        const textarea = document.createElement('textarea');
        textarea.value = response;
        textarea.style.position = 'fixed';
        textarea.style.opacity = '0';
        document.body.append(textarea);
        textarea.select();
        if (!document.execCommand('copy')) throw new Error('Clipboard API unavailable');
        textarea.remove();
      }
      relayStore.showToast('Last response copied.');
    } catch {
      relayStore.showToast('Clipboard access failed. Check browser permissions.', true);
    }
  }

  function jumpToBottom() {
    terminalElement.scrollTop = terminalElement.scrollHeight;
    jumpVisible = false;
  }

  function handleScroll() {
    if (terminalElement.scrollHeight - terminalElement.scrollTop - terminalElement.clientHeight < 48) jumpVisible = false;
  }

  async function filesSelected(files: FileList | File[]) {
    for (const file of [...files].filter((item) => item.type.startsWith('image/'))) {
      uploadStatus = `Uploading ${file.name || 'image'}…`;
      uploadError = false;
      try {
        const path = await relayStore.uploadImage(agent, file);
        const prefix = composer && !composer.endsWith('\n') ? '\n' : '';
        composer += `${prefix}Image: ${path}\n`;
        uploadStatus = `Image attached: ${path.split(/[\\/]/).pop() || 'image'}`;
      } catch (error) {
        uploadStatus = (error as Error).message;
        uploadError = true;
      }
    }
  }

  function paste(event: ClipboardEvent) {
    const files = [...(event.clipboardData?.items || [])]
      .filter((item) => item.kind === 'file' && item.type.startsWith('image/'))
      .map((item) => item.getAsFile())
      .filter((file): file is File => Boolean(file));
    if (!files.length) return;
    event.preventDefault();
    void filesSelected(files);
  }

  function openNext() {
    if (nextBlocked) replaceView({ view: 'terminal', paneId: nextBlocked.pane_id });
  }
</script>

<main
  class:has-actions={blocked || nextBlocked}
  class:question-only={questionMode}
  class="terminal-view"
  aria-label={`${questionMode ? 'Questions' : 'Terminal'} for ${agent.project || agent.name || agent.agent || 'agent'}`}
>
  {#if questionMode && interaction}
    <QuestionForm {agent} {interaction} responding={responding.has(agent.pane_id)} />
  {:else}
  <div class="terminal-toolbar">
    <Button variant="ghost" size="icon" aria-label="Refresh terminal" onclick={() => relayStore.readPane(agent)}>↻</Button>
  </div>
  <div
    class="term-content"
    bind:this={terminalElement}
    role="log"
    aria-label="Agent terminal output"
    onscroll={handleScroll}
  >
    <!-- renderTerminalContent escapes relay text before producing controlled ANSI spans. -->
    {@html renderedHtml}
  </div>
  {#if jumpVisible}
    <button class="jump-bottom" aria-label="Jump to latest output" onclick={jumpToBottom}>↓</button>
  {/if}

  <div class="terminal-bottom" onfocusin={focusComposer} onfocusout={blurComposer}>
    {#if slashMenuOpen}
      <section class="slash-command-popover" aria-label="Command suggestions">
        <header class="slash-command-header" aria-hidden="true">
          <strong>Commands</strong>
          {#if !slashCatalogLoading && !slashCatalogUnavailable}
            <span>{filteredSlashCommands.length} matching</span>
          {:else}
            <span>Type to filter</span>
          {/if}
        </header>
        {#if slashCatalogLoading}
          <p class="slash-command-status" role="status">Loading commands…</p>
        {:else if slashCatalogUnavailable}
          <p class="slash-command-status">Suggestions unavailable — you can still send this command.</p>
        {:else if !filteredSlashCommands.length}
          <p class="slash-command-status">No matching command — you can still send it.</p>
        {/if}
        <div
          id="slash-command-options"
          class="slash-command-menu"
          role="listbox"
          aria-label="Slash commands"
          aria-busy={slashCatalogLoading}
        >
          {#each filteredSlashCommands as entry, index (entry.command)}
            <button
              id={`slash-command-option-${index}`}
              type="button"
              role="option"
              tabindex="-1"
              class:active={index === effectiveSlashIndex}
              aria-selected={index === effectiveSlashIndex}
              onpointerdown={(event) => event.preventDefault()}
              onpointerenter={() => { activeSlashIndex = index; }}
              onclick={() => selectSlashCommand(entry)}
            >
              <span class="slash-command-name">
                <strong>{entry.command}</strong>
                {#if entry.argument_hint}<small>{entry.argument_hint}</small>{/if}
              </span>
              <span class="slash-command-description">{entry.description}</span>
              {#if entry.source !== 'builtin'}<em class="slash-command-source">{entry.source}</em>{/if}
            </button>
          {/each}
        </div>
        {#if !slashCatalogLoading && slashCatalog.truncated}
          <p class="slash-command-limit">More commands are available; keep typing to narrow the list.</p>
        {/if}
      </section>
    {/if}
    <div class="term-input">
      <Button variant="ghost" size="icon" disabled={blocked} aria-label="Attach image" onclick={() => fileInput.click()}>
        <svg class="button-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
          <rect x="3" y="4" width="18" height="16" rx="2"></rect>
          <circle cx="8.5" cy="9" r="1.5"></circle>
          <path d="m4 17 4.5-4.5 3.5 3.5 2.5-2.5L20 19"></path>
        </svg>
      </Button>
      <div class:awaiting-approval={blocked && !composerFocused} class:has-text={Boolean(composer)} class="composer-field">
        <textarea
          bind:this={composerElement}
          bind:value={composer}
          rows="1"
          disabled={blocked && !composerFocused}
          placeholder={blocked ? 'Approval pending — use buttons' : 'Type…'}
          role="combobox"
          aria-label="Prompt"
          aria-autocomplete="list"
          aria-haspopup="listbox"
          aria-expanded={slashMenuOpen}
          aria-controls={slashMenuOpen ? 'slash-command-options' : undefined}
          aria-activedescendant={slashMenuOpen && effectiveSlashIndex >= 0 ? `slash-command-option-${effectiveSlashIndex}` : undefined}
          autocomplete="off"
          autocorrect="on"
          autocapitalize="sentences"
          spellcheck="true"
          enterkeyhint="enter"
          oninput={composerInput}
          onkeydown={keydown}
          onpaste={paste}
        ></textarea>
        {#if composer}<button class="input-clear" aria-label="Clear prompt text" onclick={() => { composer = ''; dismissedSlashQuery = null; activeSlashIndex = 0; }}>×</button>{/if}
      </div>
      <Button size="icon" disabled={!composer.replace(/[\r\n]+$/g, '') || blocked} aria-label="Send prompt" onclick={sendPrompt}>➤</Button>
      <input bind:this={fileInput} type="file" accept="image/*" multiple hidden onchange={(event) => { void filesSelected(event.currentTarget.files || []); event.currentTarget.value = ''; }} />
    </div>
    {#if uploadStatus}<p class:error={uploadError} class="upload-status" role="status">{uploadStatus}</p>{/if}

    {#if blocked && !responding.has(agent.pane_id)}
      <div class="quick-actions" aria-label="Approval choices">
        {#each options as option, index (`${index}:${option}`)}
          <Button
            variant={approvalButtonTone(option, index, options.length) === 'deny' ? 'danger' : approvalButtonTone(option, index, options.length) === 'trust' ? 'trust' : 'default'}
            onclick={() => relayStore.respond(agent, index, options.length, option)}
          >{option}</Button>
        {/each}
        {#if nextBlocked}<Button variant="secondary" onclick={openNext}>Next blocked →</Button>{/if}
      </div>
    {:else if nextBlocked}
      <div class="quick-actions"><Button variant="secondary" onclick={openNext}>Next blocked →</Button></div>
    {/if}

    <div class="term-keys">
      <Button variant="secondary" size="sm" onclick={() => sendKeys(['Escape'], 'Cancelled prompt')}>Esc</Button>
      <Button variant="secondary" size="sm" onclick={() => sendKeys(['Tab'])}>Tab</Button>
      <Button variant="secondary" size="sm" aria-label="Copy last agent response" onclick={copyResponse}>Copy</Button>
      <span class="spacer"></span>
      <div class="arrow-menu">
        <Button variant="secondary" size="sm" aria-label="Arrow keys" aria-expanded={arrowsOpen} onclick={() => { arrowsOpen = !arrowsOpen; }}>
          <svg class="button-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
            <path d="M12 2v20M2 12h20"></path>
            <path d="m8 6 4-4 4 4M8 18l4 4 4-4M6 8l-4 4 4 4M18 8l4 4-4 4"></path>
          </svg>
        </Button>
        {#if arrowsOpen}
          <div class="arrow-popup">
            <span></span><button aria-label="Up" onclick={() => sendKeys(['Up'])}>↑</button><span></span>
            <button aria-label="Left" onclick={() => sendKeys(['Left'])}>←</button><span></span><button aria-label="Right" onclick={() => sendKeys(['Right'])}>→</button>
            <span></span><button aria-label="Down" onclick={() => sendKeys(['Down'])}>↓</button><span></span>
          </div>
        {/if}
      </div>
      <Button variant="secondary" size="sm" aria-label="Enter" onclick={() => sendKeys(['Enter'])}>Enter</Button>
    </div>
  </div>
  {/if}
</main>
