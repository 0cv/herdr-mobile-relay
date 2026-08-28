<script lang="ts">
  import { onMount, tick, untrack } from 'svelte';
  import Button from '$components/ui/Button.svelte';
  import QuestionForm from '$components/QuestionForm.svelte';
  import {
    MAX_PANE_SIZE_COLUMNS,
    MAX_PANE_SIZE_ROWS,
    MIN_PANE_SIZE_COLUMNS,
    MIN_PANE_SIZE_ROWS,
    paneLeaseRenewalAllowed,
  } from '$lib/config';
  import {
    agentNeedsInspection,
    agentNeedsResponse,
    attentionKind,
    approvalButtonTone,
    approvalOptions,
    questionInteraction,
    sortedAgents,
  } from '$lib/agents';
  import { clearPromptDraft, loadPromptDraft, savePromptDraft } from '$lib/prompt-drafts';
  import {
    findTerminalText,
    terminalMatchFragments,
    terminalRowForOffset,
    terminalRowOffsets,
    terminalSearchText,
  } from '$lib/terminal-find';
  import { interfaceSize, terminalHeightLease, theme } from '$lib/preferences';
  import { replaceView } from '$lib/router';
  import { securityState } from '$lib/security';
  import { relayStore } from '$lib/store';
  import {
    latestCompletedResponse,
    renderedRowShift,
    stripAnsi,
    TERMINAL_SEPARATOR_TOKEN,
    renderTerminalContent,
  } from '$lib/terminal';
  import { detectTerminalMenu, terminalTextInputActive } from '$lib/terminal-menu';
  import type {
    Agent,
    SlashCommand,
    SlashCommandCatalog,
    TerminalFrame,
  } from '$lib/types';
  import type { RenderedTerminalRow } from '$lib/terminal';
  import { VirtualTerminalIndex } from '$lib/virtual-terminal';

  const connections = relayStore.connections;

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

  interface VirtualTerminalAnchor {
    index: number;
    offset: number;
    text: string;
  }


  interface QueuedKeyCommand {
    keys: string[];
    label: string;
    resolve: (sent: boolean) => void;
  }
  let terminalElement = $state<HTMLDivElement>(null!);
  let cellMeasureElement = $state<HTMLSpanElement>(null!);
  let fileInput = $state<HTMLInputElement>(null!);
  let modifierInputElement = $state<HTMLInputElement>(null!);
  let findInputElement = $state<HTMLInputElement>(null!);
  let composerElement = $state<HTMLTextAreaElement>(null!);
  let transcriptElement = $state<HTMLTextAreaElement>(null!);
  let responseElement = $state<HTMLTextAreaElement>(null!);
  let agentResponsePreviewElement = $state<HTMLTextAreaElement>(null!);
  let copiedAgentResponseText = $state('');
  let composer = $state(untrack(() => loadPromptDraft(agent)));
  let composerFocused = $state(false);
  let sendingPrompt = $state(false);
  // Never handed to savePromptDraft: a no-echo prompt answer must not be
  // written to this phone's storage.
  let secretValue = $state('');
  let sendingSecret = $state(false);
  // Composer text as it stood when a no-echo prompt was first recognized, and
  // whether the user has changed it since. Recognition is a heuristic, so it
  // pauses persistence instead of deleting a draft the prompt did not author.
  let noEchoDraftBaseline: string | null = null;
  let noEchoDraftTainted = false;
  let draftPersistenceWarning = $state('');
  let historyTruncated = $state(false);
  // Pre-resize frame kept only to suppress display of transient frames while
  // the agent repaints at the new width.
  // Raw state: the effect compares frame identity, which a deep proxy breaks.
  let resizeFrameBaseline = $state.raw<TerminalFrame | undefined>(undefined);
  let displayed = $state('');
  let renderedHtml = $state('');
  let renderedRows = $state<RenderedTerminalRow[]>([]);
  let virtualHtml = $state('');
  let virtualTopHeight = $state(0);
  let virtualBottomHeight = $state(0);
  let virtualContentColumns = $state(0);
  let virtualStart = 0;
  let virtualEnd = 0;
  let virtualLayoutSignature = '';
  let virtualStickToBottom = true;
  let virtualScrollResetPending = false;
  let pendingResizeAnchor: VirtualTerminalAnchor | null = null;
  let pendingResizeStick: boolean | null = null;
  let pendingLayoutStick: boolean | null = null;
  let virtualScrollTop = 0;
  let virtualClientHeight = 0;
  let virtualWindowFrame = 0;
  let virtualRowObserver: ResizeObserver | undefined;
  let virtualHeightCache = new Map<string, number>();
  const virtualIndex = new VirtualTerminalIndex();
  let lastFormat = '';
  let lastContent = '';
  let lastPreserveLayout = false;
  let lastPreserveLineEnds = false;
  let jumpVisible = $state(false);
  let arrowsOpen = $state(false);
  let fkeysOpen = $state(false);
  let findOpen = $state(false);
  let findQuery = $state('');
  let activeFindIndex = $state(-1);
  let ctrlArmed = $state(false);
  let shiftArmed = $state(false);
  let altArmed = $state(false);
  let keyFeedback = $state('');
  let keyFeedbackError = $state(false);
  let keySending = $state(false);
  let uploadStatus = $state('');
  let uploadError = $state(false);
  let copyingAgentResponse = $state(false);
  let paneSizeLeaseError = $state('');
  let requestedPaneId = '';
  let slashCatalog = $state<SlashCommandCatalog>({ commands: [], truncated: false });
  let slashCatalogLoading = $state(true);
  let slashCatalogUnavailable = $state(false);
  let activeSlashIndex = $state(0);
  let dismissedSlashQuery = $state<string | null>(null);
  let dismissedMenuSignature = $state('');
  const CELL_MEASURE_TEXT = '0000000000';
  const PANE_SIZE_LEASE_REFRESH_MS = 10_000;
  const PANE_REALTIME_RESYNC_MS = 15_000;
  // Herdr's key parser covers f1..f24; the pad exposes the range phones need.
  const FUNCTION_KEYS = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12];
  const RESPONSE_COPY_AGENT_IDS = new Set([
    'claude', 'claudecode', 'codex', 'openaicodex', 'kimi', 'kimicode',
    'omp', 'ohmypi', 'pi', 'picodingagent', 'qoder', 'qodercli',
  ]);
  function responseCopyProfileSupported(agentName: unknown): boolean {
    const normalized = String(agentName || '').trim().toLocaleLowerCase().replace(/\s+/g, '').replace(/-/g, '');
    return RESPONSE_COPY_AGENT_IDS.has(normalized);
  }
  let componentMounted = false;
  let leaseGeneration = 0;
  let leaseInFlight = false;
  let leaseTarget: Agent | null = null;
  let lastLeasedColumns = $state(0);
  let lastLeasedRows = 0;
  let renderedResizeColumns = $state(0);
  let measuredCellWidth = $state(0);
  let queuedLease: { columns: number; rows: number; force: boolean } | null = null;
  // Set when the page goes hidden; renewals continue within the grace so a
  // desktop browser that reports occluded windows as hidden does not lapse
  // the lease on every app switch.
  let hiddenAt = 0;
  // A locked app is showing this pane to nobody who has verified, so it counts
  // as hidden for reads and watches. The connection now survives the lock, so
  // without this the terminal would keep streaming behind the unlock screen.
  const paneVisible = () => document.visibilityState === 'visible' && !$securityState.locked;
  // The lease needs its own gate: being locked is not the same as being
  // briefly hidden. The grace window below exists so an app switch does not
  // resize the computer's pane, but an unverified holder must not keep it
  // narrowed at all, so a lock skips the grace instead of entering it. The
  // relay's TTL hands the size back if the lock outlasts it.
  const paneLeaseAllowed = () => !$securityState.locked
    && paneLeaseRenewalAllowed(document.visibilityState === 'visible', hiddenAt, Date.now());
  // A lock flip changes what "visible" means for this pane, and no visibility
  // event comes with it, so the pane handler is called directly.
  let paneVisibilityChanged: (() => void) | null = null;
  let paneLocked = false;
  const keyQueue: QueuedKeyCommand[] = [];
  let keyFeedbackTimer: ReturnType<typeof setTimeout> | undefined;
  let keyReadTimer: ReturnType<typeof setTimeout> | undefined;

  const responsePending = $derived(agentNeedsResponse(agent));
  const approvalMode = $derived(responsePending && attentionKind(agent) === 'approval');
  const inspectionMode = $derived(agentNeedsInspection(agent));
  const inputLocked = $derived(responsePending || inspectionMode);
  const interaction = $derived(questionInteraction(agent));
  const questionMode = $derived(Boolean(responsePending && attentionKind(agent) === 'question' && interaction));
  const resizeSessionActive = $derived(
    Boolean($connections.get(agent.relay_id)?.capabilities.includes('pane_size_lease')),
  );
  const options = $derived(approvalOptions(agent));
  const nextBlocked = $derived(sortedAgents(allAgents.filter((item) => agentNeedsResponse(item) && item.pane_id !== agent.pane_id))[0]);
  const slashQuery = $derived(composer.startsWith('/') && !/\s/.test(composer) ? composer.slice(1).toLocaleLowerCase() : null);
  const filteredSlashCommands = $derived.by(() => {
    if (slashQuery === null) return [];
    if (!slashQuery) return slashCatalog.commands;
    return slashCatalog.commands.filter((entry) => entry.command.slice(1).toLocaleLowerCase().startsWith(slashQuery));
  });
  const effectiveSlashIndex = $derived(filteredSlashCommands.length
    ? Math.min(activeSlashIndex, filteredSlashCommands.length - 1)
    : -1);
  const slashMenuOpen = $derived(!inputLocked
    && !questionMode
    && slashQuery !== null
    && dismissedSlashQuery !== composer);
  const terminalPlainText = $derived(
    stripAnsi(displayed)
      .replaceAll(TERMINAL_SEPARATOR_TOKEN, '────────'),
  );
  const terminalTextMode = $derived(inspectionMode && terminalTextInputActive(terminalPlainText));
  const composerLocked = $derived(responsePending || (inspectionMode && !terminalTextMode));
  // The relay recognizes the prompt; that recognition is what opens the masked
  // input, even while the generic composer stays locked for inspection.
  const noEchoActive = $derived(Boolean(frame?.paneId === agent.pane_id && frame?.noEcho));
  const noEchoPrompt = $derived(noEchoActive ? frame?.noEchoPrompt || 'Password:' : '');
  const secretInputSupported = $derived(
    Boolean($connections.get(agent.relay_id)?.capabilities.includes('secret_input')),
  );
  const secretMode = $derived(noEchoActive && secretInputSupported);
  const terminalMenu = $derived(detectTerminalMenu(terminalPlainText));
  const visibleTerminalMenu = $derived(
    !approvalMode
    && !questionMode
    && terminalMenu
    && terminalMenu.signature !== dismissedMenuSignature
      ? terminalMenu
      : null,
  );
  const terminalFindCorpus = $derived.by(() => ({
    text: terminalSearchText(renderedRows),
    offsets: terminalRowOffsets(renderedRows),
  }));
  const terminalFind = $derived(findTerminalText(terminalFindCorpus.text, findQuery.trim()));
  const armedModifierLabel = $derived([
    ctrlArmed ? 'Ctrl' : '',
    altArmed ? 'Alt' : '',
    shiftArmed ? 'Shift' : '',
  ].filter(Boolean).join('+'));
  const keyControlStatus = $derived(armedModifierLabel
    ? `${armedModifierLabel} armed${keyFeedback ? ` · ${keyFeedback}` : ' — choose a key or type a character'}`
    : keyFeedback);
  const agentResponseCopySupported = $derived.by(() => {
    const connection = $connections.get(agent.relay_id);
    return Boolean(
      connection?.capabilities.includes('agent_response_copy')
      && responseCopyProfileSupported(agent.agent),
    );
  });
  const terminalCopyText = $derived(latestCompletedResponse(frame?.content || ''));
  const terminalContentStyle = $derived.by(() => {
    // Every width is emitted in px of the measured probe cell, never in ch:
    // Safari's Core Text port resolves 1ch against different font metrics
    // than the rendered glyph advance, so an Nch cap wraps short of N cells.
    // Before the probe is measured no cap is emitted at all, so the declared
    // fallbacks (100% for the wrap cap, auto for the row background) apply
    // instead of a ch-derived width that would be wrong on those engines.
    if (measuredCellWidth <= 0) return undefined;
    const capColumns = lastLeasedColumns || renderedResizeColumns;
    const styles = [`--terminal-cell-width: ${measuredCellWidth.toFixed(4)}px`];
    if (resizeSessionActive && capColumns) {
      styles.push(`--terminal-width: ${(capColumns * measuredCellWidth).toFixed(4)}px`);
    }
    if (virtualContentColumns > 0) {
      styles.push(`--terminal-content-width: ${(virtualContentColumns * measuredCellWidth).toFixed(4)}px`);
    }
    return styles.join(';');
  });

  const NOT_PERSISTED_AT_HIDDEN_PROMPT =
    'Not saved on this phone: typed while the terminal was hiding its input.';

  $effect(() => {
    if (noEchoActive) {
      // Persistence pauses so nothing typed at a hidden prompt reaches storage.
      // The stored draft stays: it predates the prompt, and recognition is a
      // heuristic that must not be able to destroy the user's own text.
      if (noEchoDraftBaseline === null) noEchoDraftBaseline = composer;
      if (composer !== noEchoDraftBaseline) noEchoDraftTainted = true;
      draftPersistenceWarning = noEchoDraftTainted ? NOT_PERSISTED_AT_HIDDEN_PROMPT : '';
      return;
    }
    noEchoDraftBaseline = null;
    if (!composer) noEchoDraftTainted = false;
    if (noEchoDraftTainted) {
      // Authored while the prompt was up: it may be the secret in the wrong
      // field, so it stays in memory until the composer is cleared or sent.
      draftPersistenceWarning = NOT_PERSISTED_AT_HIDDEN_PROMPT;
      return;
    }
    const result = savePromptDraft(agent, composer);
    draftPersistenceWarning = result === 'too-large'
      ? 'This draft is too large to persist; it survives pane switches but not closing the app.'
      : result === 'unavailable'
        ? 'Draft persistence is unavailable; keep this page open.'
        : '';
  });

  $effect(() => {
    if (secretMode) return;
    untrack(() => { secretValue = ''; });
  });

  $effect(() => {
    const count = terminalFind.matches.length;
    const query = findQuery.trim();
    untrack(() => {
      if (!query || !count) {
        activeFindIndex = -1;
        return;
      }
      if (activeFindIndex < 0 || activeFindIndex >= count) activeFindIndex = 0;
    });
  });

  $effect(() => {
    const highlightState = [
      findOpen,
      findQuery,
      activeFindIndex,
      virtualHtml,
      terminalFind.matches.length,
    ];
    void highlightState;
    void tick().then(applyTerminalFindHighlights);
  });

  // The ANSI palette follows the theme's terminal scheme, so a theme change
  // must repaint the cached frame rather than wait for the next output.
  $effect(() => {
    void $theme;
    untrack(() => {
      if (!lastContent) return;
      const next = frame;
      if (!next || next.paneId !== agent.pane_id) return;
      lastContent = '';
      void applyFrame(next, lastPreserveLayout, lastPreserveLineEnds);
    });
  });

  $effect(() => {
    const next = frame;
    const matchingFrame = next?.paneId === agent.pane_id ? next : undefined;
    const preserve = true;
    const preserveLineEnds = !resizeSessionActive;
    // A frame read while the agent repaints at a new width is transient. Keep
    // the phone's last stable frame painted until the new stable frame lands.
    const waitingForResizedFrame = resizeSessionActive
      && !paneSizeLeaseError
      && (lastLeasedColumns === 0
        || (Boolean(resizeFrameBaseline)
          && (next === resizeFrameBaseline || next?.resizeSettling === true)));
    const cachedFrame = waitingForResizedFrame
      && matchingFrame
      && matchingFrame.resizeSettling !== true
      && (!resizeFrameBaseline || matchingFrame === resizeFrameBaseline)
        ? matchingFrame
        : undefined;
    if (waitingForResizedFrame) {
      untrack(() => {
        if (pendingResizeStick !== null) return;
        pendingResizeStick = virtualStickToBottom;
        pendingResizeAnchor = virtualStickToBottom
          ? null
          : currentVirtualAnchor(terminalElement?.scrollTop || 0);
      });
      if (cachedFrame) {
        historyTruncated = Boolean(cachedFrame.truncated);
        untrack(() => { void applyFrame(cachedFrame, preserve, preserveLineEnds) });
        return;
      }
      if (renderedRows.length) return;
    }
    if (!matchingFrame || waitingForResizedFrame) {
      untrack(() => {
        if (!waitingForResizedFrame) {
          pendingResizeStick = null;
          pendingResizeAnchor = null;
        }
        const message = waitingForResizedFrame ? 'Resizing terminal…' : 'Loading…';
        const rendered = renderTerminalContent(message, 'plain');
        displayed = rendered.display;
        renderedHtml = rendered.html;
        renderedRows = rendered.rows;
        resetVirtualRows(Number.POSITIVE_INFINITY);
        lastFormat = '';
        lastContent = '';
        jumpVisible = false;
      });
      return;
    }
    if (resizeSessionActive && lastLeasedColumns > 0) renderedResizeColumns = lastLeasedColumns;
    historyTruncated = Boolean(matchingFrame.truncated);
    untrack(() => { void applyFrame(matchingFrame, preserve, preserveLineEnds) });
  });

  $effect(() => {
    const paneId = agent.pane_id;
    const connected = $connections.get(agent.relay_id)?.status === 'connected';
    if (!connected) {
      requestedPaneId = '';
      return;
    }
    if (resizeSessionActive && !paneSizeLeaseError) return;
    if (paneId === requestedPaneId) return;
    requestedPaneId = paneId;
    relayStore.readPane(agent);
  });

  $effect(() => {
    const locked = $securityState.locked;
    if (locked === paneLocked) return;
    paneLocked = locked;
    paneVisibilityChanged?.();
  });

  $effect(() => {
    const connection = $connections.get(agent.relay_id);
    const interfaceSizeValue = $interfaceSize;
    const paneId = agent.pane_id;
    void interfaceSizeValue;
    if (questionMode) {
      releasePaneSizeLease(componentMounted);
      paneSizeLeaseError = '';
      return;
    }
    if (connection?.status !== 'connected') {
      discardPaneSizeLease();
      paneSizeLeaseError = '';
      return;
    }
    if (!connection.capabilities.includes('pane_size_lease')) {
      discardPaneSizeLease();
      paneSizeLeaseError = 'Resize Session is unavailable on this relay.';
      return;
    }
    if (leaseTarget && leaseTarget.pane_id !== paneId) releasePaneSizeLease(componentMounted);
    paneSizeLeaseError = '';
    void tick().then(() => requestPaneSizeLease(false));
  });

  $effect.pre(() => {
    const interfaceSizeValue = $interfaceSize;
    void interfaceSizeValue;
    if (!terminalElement) return;
    virtualStickToBottom = terminalElement.scrollHeight
      - terminalElement.scrollTop
      - terminalElement.clientHeight < 48;
    pendingLayoutStick = virtualStickToBottom;
  });

  function rememberVirtualScrollGeometry(element: HTMLElement) {
    virtualScrollTop = element.scrollTop;
    virtualClientHeight = element.clientHeight;
  }

  function resetVirtualScroll(element: HTMLElement, stick: boolean) {
    virtualScrollResetPending = true;
    virtualLayoutSignature = '';
    const nextTop = resetVirtualRows(stick ? Number.POSITIVE_INFINITY : element.scrollTop);
    void tick().then(() => {
      element.scrollTop = stick ? element.scrollHeight : nextTop;
      rememberVirtualScrollGeometry(element);
      if (stick) virtualStickToBottom = true;
      virtualScrollResetPending = false;
    });
  }


  $effect(() => {
    const element = terminalElement;
    const interfaceSizeValue = $interfaceSize;
    void interfaceSizeValue;
    if (!element || typeof ResizeObserver === 'undefined') return;
    let previousWidth = element.clientWidth;
    let previousHeight = element.clientHeight;
    untrack(() => {
      if (!renderedRows.length) return;
      const stick = virtualStickToBottom;
      resetVirtualScroll(element, stick);
    });
    const observer = new ResizeObserver(() => {
      const nextWidth = element.clientWidth;
      const nextHeight = element.clientHeight;
      const widthChanged = Math.abs(nextWidth - previousWidth) >= 1;
      const heightChanged = Math.abs(nextHeight - previousHeight) >= 1;
      if (renderedRows.length && widthChanged) {
        const stick = virtualStickToBottom
          || element.scrollHeight - element.scrollTop - element.clientHeight < 48;
        virtualStickToBottom = stick;
        resetVirtualScroll(element, stick);
      } else if (heightChanged && virtualStickToBottom) {
        jumpToBottom();
      } else {
        scheduleVirtualWindow();
      }
      previousWidth = nextWidth;
      previousHeight = nextHeight;
      requestPaneSizeLease(false);
    });
    observer.observe(element);
    return () => observer.disconnect();
  });

  $effect(() => {
    if (!slashMenuOpen || effectiveSlashIndex < 0) return;
    const optionId = `slash-command-option-${effectiveSlashIndex}`;
    void tick().then(() => document.getElementById(optionId)?.scrollIntoView?.({ block: 'nearest' }));
  });

  $effect(() => {
    const resizeFor = [composer, $interfaceSize];
    void tick().then(() => {
      if (resizeFor[0] === composer && resizeFor[1] === $interfaceSize) resizeComposer();
    });
  });

  onMount(() => {
    let mounted = true;
    componentMounted = true;
    void relayStore.loadSlashCommands(agent).then((catalog) => {
      if (!mounted) return;
      slashCatalog = catalog;
      slashCatalogUnavailable = false;
    }).catch(() => {
      if (mounted) slashCatalogUnavailable = true;
    }).finally(() => {
      if (mounted) slashCatalogLoading = false;
    });
    const measurePane = () => requestPaneSizeLease(false);
    const realtimeDeltaEnabled = () => Boolean(
      $connections.get(agent.relay_id)?.capabilities.includes('pane_realtime_delta'),
    );
    let lastRefreshAt = Date.now();
    const visibilityChanged = () => {
      if (!paneVisible()) {
        // Traffic stops while hidden, but lease renewals keep going for a
        // bounded grace: desktop Safari reports an occluded window as hidden,
        // so treating every app switch as departure lapsed the lease and
        // resized the shared pane twice per glance, stranding stale copies of
        // inline agents' status bars in the scrollback. Once the grace runs
        // out the renewals stop and the relay's TTL hands the size back — a
        // page hidden overnight cannot keep the pane narrow.
        hiddenAt = Date.now();
        relayStore.unwatchPane(agent);
        return;
      }
      hiddenAt = 0;
      lastRefreshAt = Date.now();
      relayStore.readPane(agent);
      relayStore.watchPane(agent);
      // Re-lease at once: if the lease lapsed while asleep, the pane is back
      // at the desktop width until this lands.
      requestPaneSizeLease(true);
    };
    paneVisibilityChanged = visibilityChanged;
    const findShortcut = (event: KeyboardEvent) => {
      if (questionMode
        || event.altKey
        || (!event.ctrlKey && !event.metaKey)
        || event.key.toLocaleLowerCase() !== 'f') return;
      event.preventDefault();
      openTerminalFind();
    };
    window.addEventListener('keydown', findShortcut);
    window.addEventListener('resize', measurePane);
    window.visualViewport?.addEventListener('resize', measurePane);
    document.addEventListener('visibilitychange', visibilityChanged);
    const refresh = setInterval(() => {
      if (!paneVisible()) return;
      const refreshInterval = realtimeDeltaEnabled() ? PANE_REALTIME_RESYNC_MS : 3_000;
      if (Date.now() - lastRefreshAt < refreshInterval) return;
      lastRefreshAt = Date.now();
      relayStore.readPane(agent);
    }, 3_000);
    if (paneVisible()) relayStore.watchPane(agent);
    const refreshPaneSizeLease = setInterval(
      () => {
        if (paneLeaseAllowed()) requestPaneSizeLease(true);
      },
      PANE_SIZE_LEASE_REFRESH_MS,
    );
    void tick().then(measurePane);
    return () => {
      mounted = false;
      componentMounted = false;
      window.removeEventListener('resize', measurePane);
      window.visualViewport?.removeEventListener('resize', measurePane);
      document.removeEventListener('visibilitychange', visibilityChanged);
      paneVisibilityChanged = null;
      window.removeEventListener('keydown', findShortcut);
      clearInterval(refresh);
      clearInterval(refreshPaneSizeLease);
      if (keyFeedbackTimer) clearTimeout(keyFeedbackTimer);
      if (keyReadTimer) clearTimeout(keyReadTimer);
      for (const command of keyQueue.splice(0)) command.resolve(false);
      relayStore.unwatchPane(agent);
      releasePaneSizeLease(false);
      virtualRowObserver?.disconnect();
      if (virtualWindowFrame) cancelAnimationFrame(virtualWindowFrame);
    };
  });

  async function applyFrame(
    next: TerminalFrame,
    preserve = true,
    preserveLineEnds = preserve && !resizeSessionActive,
  ) {
    const layoutChanged = preserve !== lastPreserveLayout
      || preserveLineEnds !== lastPreserveLineEnds;
    if (next.content === lastContent && next.format === lastFormat && !layoutChanged) return;
    const rendered = renderTerminalContent(
      next.content,
      next.format,
      preserve,
      preserveLineEnds,
      resizeSessionActive ? lastLeasedColumns : 0,
    );
    lastContent = next.content;
    if (rendered.display === displayed && rendered.html === renderedHtml
      && next.format === lastFormat && !layoutChanged) return;
    const frameStick = virtualStickToBottom || Boolean(terminalElement
      && terminalElement.scrollHeight - terminalElement.scrollTop - terminalElement.clientHeight < 48);
    const stick = resizeSessionActive && pendingResizeStick !== null
      ? pendingResizeStick
      : layoutChanged && pendingLayoutStick !== null
        ? pendingLayoutStick
        : frameStick;
    const previousTop = terminalElement?.scrollTop || 0;
    let previousAnchor = stick
      ? null
      : pendingResizeAnchor || currentVirtualAnchor(previousTop);
    // Rows cropped from the front shift every index; keep the anchor on the
    // same row.
    const rowShift = previousAnchor
      ? renderedRowShift(renderedRows, rendered.rows)
      : 0;
    if (previousAnchor && rowShift) {
      previousAnchor = { ...previousAnchor, index: Math.max(0, previousAnchor.index - rowShift) };
    }
    pendingResizeStick = null;
    pendingResizeAnchor = null;
    if (layoutChanged) pendingLayoutStick = null;
    virtualStickToBottom = stick;
    virtualScrollResetPending = Boolean(terminalElement);
    displayed = rendered.display;
    renderedHtml = rendered.html;
    renderedRows = rendered.rows;
    lastFormat = next.format;
    lastPreserveLayout = preserve;
    lastPreserveLineEnds = preserveLineEnds;
    const nextTop = resetVirtualRows(
      stick ? Number.POSITIVE_INFINITY : previousTop,
      previousAnchor,
    );
    await tick();
    if (!terminalElement) {
      virtualScrollResetPending = false;
      return;
    }
    if (layoutChanged) terminalElement.scrollLeft = 0;
    if (stick) {
      terminalElement.scrollTop = terminalElement.scrollHeight;
      jumpVisible = false;
      virtualStickToBottom = true;
    } else {
      terminalElement.scrollTop = nextTop;
      jumpVisible = true;
    }
    rememberVirtualScrollGeometry(terminalElement);
    virtualScrollResetPending = false;
    observeVirtualRows();
  }

  function terminalScreenOffset(): number {
    return terminalElement?.querySelector<HTMLElement>('.term-screen')?.offsetTop || 0;
  }

  function currentVirtualAnchor(scrollTop: number): VirtualTerminalAnchor | null {
    if (!Number.isFinite(scrollTop) || !virtualIndex.length) return null;
    if (terminalElement) {
      const viewport = terminalElement.getBoundingClientRect();
      const element = [...terminalElement.querySelectorAll<HTMLElement>('[data-terminal-row]')]
        .find((row) => {
          const bounds = row.getBoundingClientRect();
          return bounds.bottom > viewport.top && bounds.top < viewport.bottom;
        });
      const index = Number.parseInt(element?.dataset.terminalRow || '', 10);
      if (element && Number.isInteger(index)) {
        return {
          index,
          offset: Math.max(0, viewport.top - element.getBoundingClientRect().top),
          text: renderedRows[index]?.text || '',
        };
      }
    }
    const contentTop = terminalScreenOffset();
    const index = virtualIndex.indexAt(Math.max(0, scrollTop - contentTop));
    return {
      index,
      offset: Math.max(0, scrollTop - contentTop - virtualIndex.offset(index)),
      text: renderedRows[index]?.text || '',
    };
  }

  function matchingAnchorIndex(anchor: VirtualTerminalAnchor): number {
    const fallback = Math.min(anchor.index, Math.max(0, renderedRows.length - 1));
    const target = anchor.text.trim();
    if (target.length < 4) return fallback;
    let bestIndex = fallback;
    let bestScore = 0;
    let bestDistance = Number.POSITIVE_INFINITY;
    for (let index = 0; index < renderedRows.length; index += 1) {
      const candidate = renderedRows[index].text.trim();
      const score = candidate === target
        ? 2
        : target.length >= 8 && (candidate.includes(target) || target.includes(candidate)) ? 1 : 0;
      const distance = Math.abs(index - anchor.index);
      if (score > bestScore || (score === bestScore && score > 0 && distance < bestDistance)) {
        bestIndex = index;
        bestScore = score;
        bestDistance = distance;
      }
    }
    return bestIndex;
  }

  function anchorOffsetLimit(anchor: VirtualTerminalAnchor, anchorIndex: number): number {
    const target = anchor.text.trim();
    let height = 0;
    let matches = 0;
    let lastSize = 0;
    for (let index = anchorIndex; index < renderedRows.length; index += 1) {
      const candidate = renderedRows[index].text.trim();
      if (!candidate || !target.includes(candidate)) break;
      lastSize = virtualIndex.size(index);
      height += lastSize;
      matches += 1;
      if (candidate === target) break;
    }
    return Math.max(0, matches > 1 ? height - lastSize : height - 1);
  }

  function resetVirtualRows(
    scrollTop: number,
    previousAnchor = currentVirtualAnchor(scrollTop),
  ) {
    const width = terminalElement?.clientWidth
      || Math.round(window.visualViewport?.width || window.innerWidth);
    const layoutSignature = [
      lastPreserveLayout ? 'preserve' : 'readable',
      resizeSessionActive ? 'resize' : 'fixed',
      lastPreserveLineEnds ? 'line-ends' : 'wrapped',
      lastLeasedColumns || renderedResizeColumns,
      $interfaceSize,
      width,
    ].join(':');
    if (layoutSignature !== virtualLayoutSignature) {
      virtualLayoutSignature = layoutSignature;
      virtualHeightCache.clear();
    } else if (virtualHeightCache.size > Math.max(2_000, renderedRows.length * 2)) {
      virtualHeightCache.clear();
    }

    const style = terminalElement ? getComputedStyle(terminalElement) : null;
    const parsedLineHeight = Number.parseFloat(style?.lineHeight || '');
    const lineHeight = Number.isFinite(parsedLineHeight) && parsedLineHeight > 0 ? parsedLineHeight : 18;
    const wrappingColumns = measuredPaneColumns()
      || lastLeasedColumns
      || renderedResizeColumns
      || 80;
    const sizes = renderedRows.map((row) => {
      const measured = virtualHeightCache.get(row.html);
      if (measured) return measured;
      if (row.separator) return lineHeight * 1.2;
      const wraps = (!lastPreserveLayout || (resizeSessionActive && !row.fixedGrid))
        ? Math.max(1, Math.ceil(row.columns / wrappingColumns))
        : 1;
      return lineHeight * wraps;
    });
    virtualIndex.reset(sizes);
    let nextTop = scrollTop;
    if (previousAnchor && virtualIndex.length) {
      const anchorIndex = matchingAnchorIndex(previousAnchor);
      const anchorOffset = Math.min(
        Math.max(0, previousAnchor.offset),
        anchorOffsetLimit(previousAnchor, anchorIndex),
      );
      nextTop = terminalScreenOffset() + virtualIndex.offset(anchorIndex) + anchorOffset;
    }

    if (!lastPreserveLayout) virtualContentColumns = 0;
    else {
      let columns = resizeSessionActive ? (lastLeasedColumns || renderedResizeColumns) : 0;
      for (const row of renderedRows) {
        if (!resizeSessionActive || row.fixedGrid) columns = Math.max(columns, row.columns);
      }
      virtualContentColumns = columns;
    }
    renderVirtualWindow(nextTop, true);
    return nextTop;
  }

  function mountedVirtualHtml(start: number, end: number): string {
    let html = '';
    for (let index = start; index < end; index += 1) {
      html += renderedRows[index].html.replace(
        '<span ',
        `<span data-terminal-row="${index}" `,
      );
    }
    return html;
  }

  function renderVirtualWindow(scrollTop: number, force = false) {
    const viewportHeight = terminalElement?.clientHeight || window.innerHeight;
    const viewportTop = Number.isFinite(scrollTop)
      ? scrollTop
      : Math.max(0, virtualIndex.total - viewportHeight);
    const range = virtualIndex.range(viewportTop, viewportHeight, viewportHeight * 1.5);
    const unchanged = range.start === virtualStart && range.end === virtualEnd;
    virtualStart = range.start;
    virtualEnd = range.end;
    virtualTopHeight = range.top;
    virtualBottomHeight = range.bottom;
    if (force || !unchanged) {
      virtualHtml = mountedVirtualHtml(range.start, range.end);
      queueVirtualRowObservation();
    }
  }

  function queueVirtualRowObservation() {
    void tick().then(observeVirtualRows);
  }

  function observeVirtualRows() {
    if (!terminalElement || typeof ResizeObserver === 'undefined') return;
    virtualRowObserver ||= new ResizeObserver(measureVirtualRows);
    virtualRowObserver.disconnect();
    for (const row of terminalElement.querySelectorAll<HTMLElement>('[data-terminal-row]')) {
      virtualRowObserver.observe(row);
    }
  }

  function measureVirtualRows(entries: ResizeObserverEntry[]) {
    if (!terminalElement || !entries.length) return;
    if (virtualScrollResetPending) return;
    const previousTop = terminalElement.scrollTop;
    const wasAtBottom = virtualStickToBottom
      || terminalElement.scrollHeight - previousTop - terminalElement.clientHeight < 48;
    const anchor = virtualIndex.indexAt(previousTop);
    let anchorDelta = 0;
    let changed = false;
    for (const entry of entries) {
      const element = entry.target as HTMLElement;
      const index = Number.parseInt(element.dataset.terminalRow || '', 10);
      if (!Number.isInteger(index) || index < 0 || index >= renderedRows.length) continue;
      const borderSize = Array.isArray(entry.borderBoxSize)
        ? entry.borderBoxSize[0]
        : entry.borderBoxSize;
      let height = borderSize?.blockSize || entry.contentRect.height;
      if (renderedRows[index].separator) {
        const style = getComputedStyle(element);
        height += (Number.parseFloat(style.marginTop) || 0)
          + (Number.parseFloat(style.marginBottom) || 0);
      }
      const delta = virtualIndex.update(index, height);
      if (!delta) continue;
      virtualHeightCache.set(renderedRows[index].html, height);
      if (index < anchor) anchorDelta += delta;
      changed = true;
    }
    if (!changed) return;
    const nextTop = wasAtBottom ? virtualIndex.total : previousTop + anchorDelta;
    virtualStickToBottom = wasAtBottom;
    virtualScrollResetPending = true;
    renderVirtualWindow(nextTop);
    void tick().then(() => {
      if (!terminalElement) {
        virtualScrollResetPending = false;
        return;
      }
      terminalElement.scrollTop = wasAtBottom ? terminalElement.scrollHeight : nextTop;
      rememberVirtualScrollGeometry(terminalElement);
      if (wasAtBottom) virtualStickToBottom = true;
      virtualScrollResetPending = false;
    });
  }

  function scheduleVirtualWindow() {
    if (virtualWindowFrame) return;
    virtualWindowFrame = requestAnimationFrame(() => {
      virtualWindowFrame = 0;
      if (terminalElement) renderVirtualWindow(terminalElement.scrollTop);
    });
  }

  function clearTerminalFindHighlights() {
    if (!terminalElement) return;
    const parents = new Set<Node>();
    for (const mark of terminalElement.querySelectorAll<HTMLElement>('mark[data-terminal-find]')) {
      if (mark.parentNode) parents.add(mark.parentNode);
      mark.replaceWith(document.createTextNode(mark.textContent || ''));
    }
    for (const parent of parents) parent.normalize();
  }

  function applyTerminalFindHighlights() {
    clearTerminalFindHighlights();
    if (!terminalElement || !findOpen || !findQuery.trim() || !terminalFind.matches.length) return;
    const byRow = new Map<number, Array<{ start: number; end: number; active: boolean }>>();
    terminalFind.matches.forEach((match, matchIndex) => {
      for (const fragment of terminalMatchFragments(renderedRows, terminalFindCorpus.offsets, match)) {
        if (fragment.row < virtualStart || fragment.row >= virtualEnd) continue;
        const fragments = byRow.get(fragment.row) || [];
        fragments.push({ start: fragment.start, end: fragment.end, active: matchIndex === activeFindIndex });
        byRow.set(fragment.row, fragments);
      }
    });
    for (const [row, fragments] of byRow) {
      const element = terminalElement.querySelector<HTMLElement>(`[data-terminal-row="${row}"]`);
      if (element) highlightTerminalRow(element, fragments);
    }
  }

  function highlightTerminalRow(
    element: HTMLElement,
    fragments: Array<{ start: number; end: number; active: boolean }>,
  ) {
    const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
    const nodes: Array<{ node: Text; start: number; end: number }> = [];
    let offset = 0;
    let node = walker.nextNode();
    while (node) {
      const text = node as Text;
      nodes.push({ node: text, start: offset, end: offset + text.data.length });
      offset += text.data.length;
      node = walker.nextNode();
    }
    for (const entry of nodes) {
      const intersections = fragments
        .map((fragment) => ({
          start: Math.max(fragment.start, entry.start) - entry.start,
          end: Math.min(fragment.end, entry.end) - entry.start,
          active: fragment.active,
        }))
        .filter((fragment) => fragment.end > fragment.start)
        .sort((left, right) => right.start - left.start);
      for (const fragment of intersections) {
        entry.node.splitText(fragment.end);
        const selected = entry.node.splitText(fragment.start);
        const mark = document.createElement('mark');
        mark.dataset.terminalFind = '';
        mark.className = `terminal-find-match${fragment.active ? ' active' : ''}`;
        selected.replaceWith(mark);
        mark.append(selected);
      }
    }
  }

  function openTerminalFind() {
    arrowsOpen = false;
    findOpen = true;
    void tick().then(() => {
      findInputElement?.focus();
      findInputElement?.select();
    });
  }

  // The header owns the Find control, so the terminal exposes opening it rather
  // than lifting findOpen out: kept here, the bar still closes with the pane.
  export function openFind() {
    openTerminalFind();
  }

  function closeTerminalFind() {
    findOpen = false;
  }

  function findInputChanged() {
    activeFindIndex = -1;
    void tick().then(() => {
      if (terminalFind.matches.length) revealFindMatch(0);
    });
  }

  function findKeydown(event: KeyboardEvent) {
    if (event.key === 'Escape') {
      event.preventDefault();
      closeTerminalFind();
      return;
    }
    if (event.key !== 'Enter') return;
    event.preventDefault();
    revealFindMatch(activeFindIndex + (event.shiftKey ? -1 : 1));
  }

  function revealFindMatch(index: number) {
    const count = terminalFind.matches.length;
    if (!terminalElement || !count) return;
    const normalized = ((index % count) + count) % count;
    activeFindIndex = normalized;
    const match = terminalFind.matches[normalized];
    const row = terminalRowForOffset(renderedRows, terminalFindCorpus.offsets, match.start);
    if (row < 0) return;
    const rowTop = terminalScreenOffset() + virtualIndex.offset(row);
    const nextTop = Math.max(0, rowTop - (terminalElement.clientHeight - virtualIndex.size(row)) / 2);
    virtualStickToBottom = false;
    virtualScrollResetPending = true;
    renderVirtualWindow(nextTop, true);
    void tick().then(() => {
      if (!terminalElement) return;
      terminalElement.scrollTop = nextTop;
      rememberVirtualScrollGeometry(terminalElement);
      virtualStickToBottom = terminalElement.scrollHeight
        - terminalElement.scrollTop
        - terminalElement.clientHeight < 48;
      jumpVisible = !virtualStickToBottom;
      virtualScrollResetPending = false;
      applyTerminalFindHighlights();
    });
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
    });
  }


  async function sendPrompt() {
    const submittedDraft = composer;
    const text = submittedDraft.replace(/[\r\n]+$/g, '');
    if (!text || composerLocked || sendingPrompt) return;
    const terminalText = terminalTextMode;
    let terminalTextInserted = false;
    sendingPrompt = true;
    composer = '';
    clearPromptDraft(agent);
    try {
      if (terminalText) {
        await relayStore.sendToAgent(agent, { type: 'send_text', text });
        terminalTextInserted = true;
        await relayStore.sendToAgent(agent, {
          type: 'send_keys',
          keys: ['Enter'],
          activity_label: 'Submitted terminal text',
        });
      } else {
        await relayStore.sendToAgent(agent, { type: 'submit_prompt', text });
      }
      relayStore.showToast(terminalText ? 'Terminal text submitted.' : 'Prompt sent.');
    } catch (error) {
      const dispatchedUnknown = typeof error === 'object'
        && error !== null
        && 'data' in error
        && typeof error.data === 'object'
        && error.data !== null
        && 'dispatched_unknown' in error.data
        && error.data.dispatched_unknown === true;
      if (!composer && !dispatchedUnknown && !terminalTextInserted) composer = submittedDraft;
      const detail = error instanceof Error
        ? error.message
        : terminalText ? 'Terminal text could not be submitted.' : 'Prompt could not be sent.';
      const recovery = terminalText && terminalTextInserted
        ? `${detail} Text remains in the terminal; use Enter to submit it.`
        : dispatchedUnknown
          ? `${detail} Check the terminal before sending again.`
          : detail;
      relayStore.showToast(recovery, true);
    } finally {
      sendingPrompt = false;
      setTimeout(() => relayStore.readPane(agent), 500);
    }
  }

  async function submitSecret() {
    const secret = secretValue;
    if (!secret || !secretMode || sendingSecret) return;
    sendingSecret = true;
    try {
      await relayStore.sendSecret(agent, secret);
      secretValue = '';
      relayStore.showToast('Password sent to the terminal.');
    } catch (error) {
      // The value stays in the field for a retry; it is never stored.
      const message = error instanceof Error && error.message
        ? error.message
        : 'The password could not be sent.';
      relayStore.showToast(message, true);
    } finally {
      sendingSecret = false;
      setTimeout(() => relayStore.readPane(agent), 500);
    }
  }

  function secretKeydown(event: KeyboardEvent) {
    if (event.isComposing || event.key !== 'Enter') return;
    event.preventDefault();
    void submitSecret();
  }

  function composerInput() {
    if (dismissedSlashQuery !== composer) dismissedSlashQuery = null;
    activeSlashIndex = 0;
  }

  function clearComposer() {
    composer = '';
    dismissedSlashQuery = null;
    activeSlashIndex = 0;
  }

  function resizeComposer() {
    if (!composerElement) return;
    composerElement.style.height = 'auto';
    const maxHeight = Number.parseFloat(getComputedStyle(composerElement).maxHeight);
    const contentHeight = composerElement.scrollHeight;
    const capped = Number.isFinite(maxHeight) && contentHeight > maxHeight;
    composerElement.style.height = `${capped ? maxHeight : contentHeight}px`;
    composerElement.style.overflowY = capped ? 'auto' : 'hidden';
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

  function sendKeys(keys: string[], activityLabel = ''): Promise<boolean> {
    return new Promise((resolve) => {
      keyQueue.push({ keys, label: activityLabel || keys.join(', '), resolve });
      void drainKeyQueue();
    });
  }

  async function drainKeyQueue() {
    if (keySending) return;
    keySending = true;
    while (keyQueue.length) {
      const command = keyQueue.shift()!;
      showKeyFeedback(`Sending ${command.label}…`);
      try {
        await relayStore.sendToAgent(agent, {
          type: 'send_keys',
          keys: command.keys,
          activity_label: command.label,
        });
        command.resolve(true);
        showKeyFeedback(`${command.label} sent`);
        if (keyReadTimer) clearTimeout(keyReadTimer);
        keyReadTimer = setTimeout(() => {
          if (componentMounted) relayStore.readPane(agent);
        }, 300);
      } catch (error) {
        command.resolve(false);
        const message = error instanceof Error ? error.message : 'Terminal keys could not be sent.';
        showKeyFeedback('Key send failed', true);
        relayStore.showToast(message, true);
        for (const queued of keyQueue.splice(0)) queued.resolve(false);
      }
    }
    keySending = false;
  }

  function showKeyFeedback(message: string, error = false) {
    if (!componentMounted) return;
    if (keyFeedbackTimer) clearTimeout(keyFeedbackTimer);
    keyFeedback = message;
    keyFeedbackError = error;
    keyFeedbackTimer = setTimeout(() => {
      keyFeedback = '';
      keyFeedbackError = false;
    }, 2_000);
  }

  async function copyTerminalOutput() {
    copiedAgentResponseText = '';
    let text = '';
    let copiedAgentResponse = false;
    if (agentResponseCopySupported) {
      copyingAgentResponse = true;
      try {
        const result = await relayStore.sendToAgent(agent, { type: 'copy_agent_response' }, 15_000);
        const remoteText = String(result.data?.text || '');
        if (!remoteText.trim()) {
          relayStore.showToast('The agent returned no response to copy.', true);
          return;
        }
        text = remoteText;
        copiedAgentResponse = true;
        copiedAgentResponseText = remoteText;
      } catch (error) {
        const message = error instanceof Error && error.message
          ? error.message
          : 'Could not copy the agent response.';
        relayStore.showToast(message, true);
        return;
      } finally {
        copyingAgentResponse = false;
      }
    }
    if (!text) text = terminalCopyText || terminalPlainText;
    if (!text.trim()) {
      relayStore.showToast('No terminal output is available to copy.', true);
      return;
    }
    const hasCompletedResponse = copiedAgentResponse || Boolean(terminalCopyText.trim());
    const target = hasCompletedResponse ? responseElement : transcriptElement;
    const copiedMessage = copiedAgentResponse
      ? 'Agent response copied.'
      : hasCompletedResponse
        ? 'Final response copied.'
        : 'Copied the visible terminal output.';
    const selectedMessage = copiedAgentResponse
      ? 'Output selected. Use your browser Copy command.'
      : hasCompletedResponse
        ? 'Final response selected. Use your browser Copy command.'
        : 'Output selected. Use your browser Copy command.';
    if (!navigator.clipboard?.writeText) {
      target.value = text;
      target.focus({ preventScroll: true });
      target.select();
      relayStore.showToast(selectedMessage);
      return;
    }
    try {
      await navigator.clipboard.writeText(text);
      relayStore.showToast(copiedMessage);
    } catch {
      target.value = text;
      target.focus({ preventScroll: true });
      target.select();
      relayStore.showToast(selectedMessage);
    }
  }

  async function copyDisplayedAgentResponse() {
    const text = copiedAgentResponseText;
    if (!text.trim()) return;
    if (!navigator.clipboard?.writeText) {
      agentResponsePreviewElement.focus({ preventScroll: true });
      agentResponsePreviewElement.select();
      relayStore.showToast('Output selected. Use your browser Copy command.');
      return;
    }
    try {
      await navigator.clipboard.writeText(text);
      relayStore.showToast('Agent response copied.');
    } catch {
      agentResponsePreviewElement.focus({ preventScroll: true });
      agentResponsePreviewElement.select();
      relayStore.showToast('Output selected. Use your browser Copy command.');
    }
  }

  function dismissCopiedAgentResponse() {
    copiedAgentResponseText = '';
  }
  function toggleModifier(which: 'ctrl' | 'alt' | 'shift') {
    arrowsOpen = false;
    fkeysOpen = false;
    if (which === 'ctrl') ctrlArmed = !ctrlArmed;
    else if (which === 'alt') altArmed = !altArmed;
    else shiftArmed = !shiftArmed;
    if (ctrlArmed || altArmed || shiftArmed) {
      modifierInputElement.value = '';
      modifierInputElement.focus();
    } else {
      modifierInputElement.blur();
    }
  }

  function toggleCtrl() {
    toggleModifier('ctrl');
  }

  function toggleAlt() {
    toggleModifier('alt');
  }

  function toggleShift() {
    toggleModifier('shift');
  }

  function modifierChord(key: string): { chord: string; label: string } | null {
    const parts: string[] = [];
    const labels: string[] = [];
    if (ctrlArmed) { parts.push('ctrl'); labels.push('Ctrl'); }
    if (altArmed) { parts.push('alt'); labels.push('Alt'); }
    if (shiftArmed) { parts.push('shift'); labels.push('Shift'); }
    if (!parts.length) return null;
    parts.push(key.toLocaleLowerCase());
    labels.push(key.length === 1 ? key.toLocaleUpperCase() : key[0].toLocaleUpperCase() + key.slice(1).toLocaleLowerCase());
    return { chord: parts.join('+'), label: labels.join('+') };
  }

  function disarmModifiers() {
    ctrlArmed = false;
    altArmed = false;
    shiftArmed = false;
    modifierInputElement.blur();
  }

  function modifierInput(event: Event) {
    const target = event.currentTarget as HTMLInputElement;
    const character = Array.from(target.value)[0] || '';
    target.value = '';
    if (!character || /\s/u.test(character)) return;
    sendTerminalKey(character);
  }

  function sendTerminalKey(key: string, plainLabel = key) {
    const result = modifierChord(key);
    void sendKeys([result?.chord || key], result?.label || plainLabel);
  }

  function sendTab() {
    sendTerminalKey('Tab');
  }

  function sendFunctionKey(number: number) {
    fkeysOpen = false;
    // Herdr parses function keys as f1..f24; the label keeps the pad readable.
    sendTerminalKey(`f${number}`, `F${number}`);
  }

  function modifierKeydown(event: KeyboardEvent) {
    if (event.key !== 'Escape') return;
    event.preventDefault();
    disarmModifiers();
  }

  function modifierBlur() {
    setTimeout(() => {
      if (document.activeElement !== modifierInputElement) {
        ctrlArmed = false;
        altArmed = false;
        shiftArmed = false;
      }
    });
  }


  function jumpToBottom() {
    virtualStickToBottom = true;
    virtualScrollResetPending = true;
    renderVirtualWindow(Number.POSITIVE_INFINITY, true);
    void tick().then(() => {
      if (!terminalElement) {
        virtualScrollResetPending = false;
        return;
      }
      terminalElement.scrollTop = terminalElement.scrollHeight;
      rememberVirtualScrollGeometry(terminalElement);
      virtualScrollResetPending = false;
      jumpVisible = false;
    });
  }

  function handleScroll() {
    if (virtualScrollResetPending) {
      rememberVirtualScrollGeometry(terminalElement);
      return;
    }
    const scrollTop = terminalElement.scrollTop;
    const scrollHeight = terminalElement.scrollHeight;
    const clientHeight = terminalElement.clientHeight;
    const bottomDistance = scrollHeight - scrollTop - clientHeight;
    const atBottom = bottomDistance < 48;
    // Only a viewport/controls height change may re-pin: content growth also
    // changes scrollHeight, and a user scrolling up during a stream must win.
    const layoutChanged = Math.abs(clientHeight - virtualClientHeight) >= 1;
    // A scroll that lands exactly at the bottom is never the user moving
    // toward history: when corrected row heights shrink the content while
    // pinned, the browser clamps scrollTop to the new maximum and fires a
    // scroll event whose position is lower than the remembered one. Reading
    // that clamp as intent dropped stick-to-bottom, so Safari — whose real
    // row heights disagree with the estimates more than Chromium's — opened
    // a growing gap above the transcript's end and fought every scroll with
    // anchor-preserving corrections (issue #11's missing bottom + flicker).
    const movedTowardHistory = !layoutChanged
      && scrollTop < virtualScrollTop - 1
      && bottomDistance > 1;
    rememberVirtualScrollGeometry(terminalElement);
    if (movedTowardHistory) {
      virtualStickToBottom = false;
      jumpVisible = true;
    } else if (atBottom) {
      if (bottomDistance > 0.5) {
        jumpToBottom();
        return;
      }
      virtualStickToBottom = true;
      jumpVisible = false;
    } else if (!virtualStickToBottom) {
      jumpVisible = true;
    } else {
      jumpToBottom();
      return;
    }
    scheduleVirtualWindow();
  }

  function paneSizeLeaseSupported(target: Agent): boolean {
    const connection = $connections.get(target.relay_id);
    return componentMounted
      && !questionMode
      && connection?.status === 'connected'
      && connection.capabilities.includes('pane_size_lease');
  }

  function paneSizeRowLeaseSupported(target: Agent): boolean {
    return $terminalHeightLease && Boolean(
      $connections.get(target.relay_id)?.capabilities.includes('pane_size_lease_rows'),
    );
  }

  // A toggle flips the next lease immediately: on adds the measured rows, off
  // renews width-only, which lifts this client's row constraint on the relay.
  $effect(() => {
    const enabled = $terminalHeightLease;
    void enabled;
    void tick().then(() => requestPaneSizeLease(false));
  });

  function measuredPaneColumns(): number | null {
    if (!terminalElement || !cellMeasureElement) return null;
    const cellWidth = cellMeasureElement.getBoundingClientRect().width / CELL_MEASURE_TEXT.length;
    const style = getComputedStyle(terminalElement);
    const horizontalPadding = (Number.parseFloat(style.paddingLeft) || 0)
      + (Number.parseFloat(style.paddingRight) || 0);
    const usableWidth = terminalElement.clientWidth - horizontalPadding;
    if (!Number.isFinite(cellWidth) || cellWidth <= 0 || usableWidth <= 0) return null;
    // The probed advance also drives every CSS width cap, so the caps stay
    // correct the moment the font or interface size changes.
    if (cellWidth !== measuredCellWidth) measuredCellWidth = cellWidth;
    return Math.min(
      MAX_PANE_SIZE_COLUMNS,
      Math.max(MIN_PANE_SIZE_COLUMNS, Math.floor(usableWidth / cellWidth)),
    );
  }

  // 0 means "do not lease rows": the relay keeps the pane's own height.
  function measuredPaneRows(): number {
    if (!terminalElement) return 0;
    const style = getComputedStyle(terminalElement);
    const lineHeight = Number.parseFloat(style.lineHeight);
    const verticalPadding = (Number.parseFloat(style.paddingTop) || 0)
      + (Number.parseFloat(style.paddingBottom) || 0);
    const usableHeight = terminalElement.clientHeight - verticalPadding;
    if (!Number.isFinite(lineHeight) || lineHeight <= 0 || usableHeight <= 0) return 0;
    return Math.min(
      MAX_PANE_SIZE_ROWS,
      Math.max(MIN_PANE_SIZE_ROWS, Math.floor(usableHeight / lineHeight)),
    );
  }

  function beginResizeSettling() {
    resizeFrameBaseline = frame;
  }

  function clearResizeSettling() {
    resizeFrameBaseline = undefined;
  }

  function discardPaneSizeLease() {
    leaseGeneration += 1;
    leaseTarget = null;
    lastLeasedColumns = 0;
    lastLeasedRows = 0;
    clearResizeSettling();
    queuedLease = null;
  }

  function releasePaneSizeLease(reportFailure: boolean) {
    const target = leaseTarget;
    discardPaneSizeLease();
    if (!target) return;
    void relayStore.releasePaneSize(target).catch((error) => {
      const connection = $connections.get(target.relay_id);
      if (reportFailure && componentMounted && connection?.status === 'connected') {
        paneSizeLeaseError = `Resize Session release failed: ${(error as Error).message}`;
      }
    });
  }

  function requestPaneSizeLease(force: boolean) {
    const target = agent;
    // A hidden page renews only within the grace window: after it, the
    // relay's lease TTL returns the desktop size, and the refocus handler
    // re-leases the moment the page is visible again.
    if (!paneLeaseAllowed()) return;
    if (!paneSizeLeaseSupported(target)) return;
    const columns = measuredPaneColumns();
    if (columns === null) {
      if (terminalElement && cellMeasureElement) {
        paneSizeLeaseError = 'Resize Session could not measure the terminal cell width.';
      }
      return;
    }
    let rows = paneSizeRowLeaseSupported(target) ? measuredPaneRows() : 0;
    // The on-screen keyboard shrinks the terminal while the user types, and
    // leasing that transient height would SIGWINCH the agent twice per
    // keyboard toggle. Every full-height redraw can strand a stale copy of a
    // bottom-anchored status bar in the scrollback, so while a text input
    // owns focus the lease keeps its resting height and may only grow; the
    // resize listeners re-measure the moment the keyboard closes.
    const active = document.activeElement;
    const typing = active instanceof HTMLTextAreaElement || active instanceof HTMLInputElement;
    if (typing && rows && lastLeasedRows && rows < lastLeasedRows) rows = lastLeasedRows;
    const sameTarget = leaseTarget?.pane_id === target.pane_id;
    if (!force && sameTarget && columns === lastLeasedColumns && rows === lastLeasedRows) return;
    if (queuedLease && queuedLease.columns === columns && queuedLease.rows === rows) {
      queuedLease = { columns, rows, force: queuedLease.force || force };
    } else queuedLease = { columns, rows, force };
    if (!leaseInFlight) void flushPaneSizeLease();
  }

  async function flushPaneSizeLease() {
    if (leaseInFlight) return;
    leaseInFlight = true;
    try {
      while (queuedLease) {
        const request = queuedLease;
        queuedLease = null;
        const target = agent;
        if (!paneSizeLeaseSupported(target)) continue;
        if (!request.force
          && leaseTarget?.pane_id === target.pane_id
          && request.columns === lastLeasedColumns
          && request.rows === lastLeasedRows) continue;
        if (leaseTarget && leaseTarget.pane_id !== target.pane_id) {
          releasePaneSizeLease(componentMounted);
        }
        const generation = leaseGeneration;
        leaseTarget = target;
        try {
          if (request.columns !== lastLeasedColumns
            || request.rows !== lastLeasedRows) beginResizeSettling();
          const applied = await relayStore.leasePaneSize(target, request.columns, request.rows);
          if (generation !== leaseGeneration
            || leaseTarget?.pane_id !== target.pane_id
            || !paneSizeLeaseSupported(target)) continue;
          const changed = applied.columns !== lastLeasedColumns
            || applied.rows !== lastLeasedRows;
          lastLeasedColumns = applied.columns;
          lastLeasedRows = applied.rows;
          paneSizeLeaseError = '';
          if (changed) {
            // The pane repaints at the new size: read again so the live
            // screen is the resized one. History stays with the relay journal.
            relayStore.readPane(target, true);
          }
        } catch (error) {
          if (generation === leaseGeneration
            && leaseTarget?.pane_id === target.pane_id
            && paneSizeLeaseSupported(target)) {
            queuedLease = null;
            lastLeasedColumns = 0;
            lastLeasedRows = 0;
            clearResizeSettling();
            paneSizeLeaseError = `Resize Session failed: ${(error as Error).message}`;
          }
        }
      }
    } finally {
      leaseInFlight = false;
      if (queuedLease) void flushPaneSizeLease();
    }
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

  function menuKeyLabel(keys: string[]): string {
    return keys.map((key) => key === ' ' ? 'Space' : key).join('+');
  }

  function openNext() {
    if (nextBlocked) replaceView({ view: 'terminal', paneId: nextBlocked.pane_id });
  }
</script>

{#snippet arrowIcon()}
  <svg class="button-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
    <path d="M12 2v20M2 12h20"></path>
    <path d="m8 6 4-4 4 4M8 18l4 4 4-4M6 8l-4 4 4 4M18 8l4 4-4 4"></path>
  </svg>
{/snippet}

<!-- Drawn rather than typed: ⇥ and ⇧ resolve to a different fallback font on
     each platform, and their glyphs sit at different heights inside the em box,
     so a text label cannot be centred for Android and the desktop at once. -->
{#snippet tabIcon()}
  <svg class="key-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
    <path d="M3 12h12M11 8l4 4-4 4M20 6v12"></path>
  </svg>
{/snippet}

{#snippet shiftIcon()}
  <svg class="key-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
    <path d="M12 4.5 4 12.5h4v7h8v-7h4z"></path>
  </svg>
{/snippet}

{#snippet copyIcon()}
  <svg class="action-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
    <rect x="9" y="9" width="11" height="12" rx="2"></rect>
    <path d="M5 15H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h10a1 1 0 0 1 1 1v1"></path>
  </svg>
{/snippet}
{#snippet findPreviousIcon()}
  <svg class="find-action-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.3" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
    <path d="m6 15 6-6 6 6"></path>
  </svg>
{/snippet}

{#snippet findNextIcon()}
  <svg class="find-action-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.3" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
    <path d="m6 9 6 6 6-6"></path>
  </svg>
{/snippet}

{#snippet findCloseIcon()}
  <svg class="find-action-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.3" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
    <path d="m6 6 12 12M18 6 6 18"></path>
  </svg>
{/snippet}


{#snippet arrowPopup()}
  {#if arrowsOpen}
    <div class="arrow-popup">
      <span aria-hidden="true"></span>
      <button aria-label="Up" onpointerdown={(event) => event.preventDefault()} onclick={() => sendTerminalKey('Up')}>↑</button>
      <span aria-hidden="true"></span>
      <button aria-label="Left" onpointerdown={(event) => event.preventDefault()} onclick={() => sendTerminalKey('Left')}>←</button>
      <span aria-hidden="true"></span>
      <button aria-label="Right" onpointerdown={(event) => event.preventDefault()} onclick={() => sendTerminalKey('Right')}>→</button>
      <span aria-hidden="true"></span>
      <button aria-label="Down" onpointerdown={(event) => event.preventDefault()} onclick={() => sendTerminalKey('Down')}>↓</button>
      <span aria-hidden="true"></span>
    </div>
  {/if}
{/snippet}

{#snippet fkeyPopup()}
  {#if fkeysOpen}
    <div class="fkey-popup" role="group" aria-label="Function keys">
      {#each FUNCTION_KEYS as number (number)}
        <button
          onpointerdown={(event) => event.preventDefault()}
          onclick={() => sendFunctionKey(number)}
        >F{number}</button>
      {/each}
    </div>
  {/if}
{/snippet}

<main
  class:has-actions={inputLocked || nextBlocked}
  class:question-only={questionMode}
  class:find-open={findOpen}
  class="terminal-view"
  aria-label={`${questionMode ? 'Questions' : 'Terminal'} for ${agent.project || agent.name || agent.agent || 'agent'}`}
>
  {#if questionMode && interaction}
    <QuestionForm {agent} {interaction} responding={responding.has(agent.pane_id)} />
    {#if keyControlStatus}
      <p class:error={keyFeedbackError} class="key-feedback" role="status" aria-live="polite">{keyControlStatus}</p>
    {/if}
    <div class="term-keys question-term-keys" aria-label="Terminal fallback keys" aria-busy={keySending}>
      <Button variant="secondary" size="sm" onclick={() => sendTerminalKey('Escape', 'Cancelled prompt')}>Esc</Button>
      <Button variant="secondary" size="sm" aria-label="Tab" title="Send Tab" onclick={sendTab}>{@render tabIcon()}</Button>
      <span class="spacer"></span>
      <div class="fkey-menu">
        <Button variant="secondary" size="sm" aria-label="Function keys" aria-expanded={fkeysOpen} onclick={() => { fkeysOpen = !fkeysOpen; arrowsOpen = false; }}>
          F keys
        </Button>
        {@render fkeyPopup()}
      </div>
      <div class="arrow-menu">
        <Button variant="secondary" size="sm" aria-label="Arrow keys" aria-expanded={arrowsOpen} onclick={() => { arrowsOpen = !arrowsOpen; fkeysOpen = false; }}>
          {@render arrowIcon()}
        </Button>
        {@render arrowPopup()}
      </div>
      <Button variant="secondary" size="sm" aria-label="Enter" onclick={() => sendTerminalKey('Enter')}>Enter</Button>
    </div>
  {/if}
  <div class:hidden={questionMode} class="terminal-view term">
  {#if findOpen}
    <section class="terminal-find" aria-label="Find in terminal">
      <input
        bind:this={findInputElement}
        bind:value={findQuery}
        type="search"
        aria-label="Find in terminal output"
        placeholder="Find in terminal"
        autocomplete="off"
        autocapitalize="none"
        spellcheck="false"
        oninput={findInputChanged}
        onkeydown={findKeydown}
      />
      {#if findQuery.trim()}
        <span class="terminal-find-count" role="status" aria-live="polite">
          {#if !terminalFind.matches.length}
            No matches
          {:else}
            {activeFindIndex + 1} of {terminalFind.matches.length}{terminalFind.truncated ? '+' : ''}
          {/if}
        </span>
      {/if}
      <Button variant="secondary" size="sm" aria-label="Previous match" disabled={!terminalFind.matches.length} onclick={() => revealFindMatch(activeFindIndex - 1)}>
        {@render findPreviousIcon()}
      </Button>
      <Button variant="secondary" size="sm" aria-label="Next match" disabled={!terminalFind.matches.length} onclick={() => revealFindMatch(activeFindIndex + 1)}>
        {@render findNextIcon()}
      </Button>
      <Button variant="ghost" size="sm" aria-label="Close find" onclick={closeTerminalFind}>
        {@render findCloseIcon()}
      </Button>
    </section>
  {/if}
  <div class="term-wrap">
  <div
    class:resize-layout={resizeSessionActive}
    class="term-content preserve-layout"
    style={terminalContentStyle}
    bind:this={terminalElement}
    role="log"
    aria-label="Agent terminal output"
    onscroll={handleScroll}
  >
    <span
      bind:this={cellMeasureElement}
      aria-hidden="true"
      style="pointer-events: none; position: absolute; visibility: hidden; white-space: pre;"
    >{CELL_MEASURE_TEXT}</span>
    <div class="term-screen" data-terminal-row-count={renderedRows.length}>
      {#if virtualTopHeight > 0}
        <span class="terminal-virtual-spacer" style={`height:${virtualTopHeight}px`} aria-hidden="true"></span>
      {/if}
      <!-- Normalized rows are escaped before controlled ANSI spans enter this bounded DOM window. -->
      {@html virtualHtml}
      {#if virtualBottomHeight > 0}
        <span class="terminal-virtual-spacer" style={`height:${virtualBottomHeight}px`} aria-hidden="true"></span>
      {/if}
    </div>
  </div>
    {#if jumpVisible}
      <button class="jump-bottom" aria-label="Jump to latest" onclick={jumpToBottom}>↓</button>
    {/if}
  </div>
  <textarea
    class="sr-only"
    aria-label="Full terminal transcript"
    readonly
    tabindex="-1"
    bind:this={transcriptElement}
    value={terminalPlainText}
  ></textarea>
  <textarea
    class="sr-only"
    aria-label="Latest final response"
    readonly
    tabindex="-1"
    bind:this={responseElement}
    value={terminalCopyText}
  ></textarea>
  {#if copiedAgentResponseText}
    <section class="agent-response-preview" aria-label="Copied agent response">
      <div class="agent-response-preview-header">
        <strong>Markdown response</strong>
        <div class="agent-response-preview-actions">
          <Button variant="secondary" size="sm" onclick={copyDisplayedAgentResponse}>Copy markdown</Button>
          <Button variant="ghost" size="sm" onclick={dismissCopiedAgentResponse}>Dismiss</Button>
        </div>
      </div>
      <textarea
        aria-label="Copied agent response markdown"
        readonly
        bind:this={agentResponsePreviewElement}
        value={copiedAgentResponseText}
      ></textarea>
    </section>
  {/if}
  <div class="terminal-copy">
    <Button
      variant="secondary"
      size="sm"
      aria-label={copyingAgentResponse ? 'Copying…' : 'Copy'}
      aria-busy={copyingAgentResponse}
      title={copyingAgentResponse ? 'Copying…' : 'Copy output'}
      disabled={copyingAgentResponse || responding.has(agent.pane_id)}
      onclick={copyTerminalOutput}
    >{@render copyIcon()}</Button>
  </div>

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
    {#if noEchoActive}
      <section class="secret-prompt" aria-label="Hidden terminal prompt">
        <p id="secret-prompt-line" role="status">
          The terminal is asking for a hidden value: <strong>{noEchoPrompt}</strong>
        </p>
        {#if secretInputSupported}
          <div class="secret-prompt-row">
            <input
              bind:value={secretValue}
              type="password"
              aria-label="Value for the hidden terminal prompt"
              aria-describedby="secret-prompt-line"
              autocomplete="off"
              autocapitalize="none"
              spellcheck="false"
              enterkeyhint="send"
              disabled={sendingSecret}
              onkeydown={secretKeydown}
            />
            <Button
              size="sm"
              disabled={!secretValue || sendingSecret}
              aria-label="Send hidden value"
              onclick={submitSecret}
            >{sendingSecret ? '…' : 'Send'}</Button>
          </div>
          <p class="hint">Typed straight into the terminal: never saved on this phone and never written to activity.</p>
        {:else}
          <p class="hint">This computer’s relay is too old to accept a hidden value from the phone; answer it at the computer.</p>
        {/if}
      </section>
    {/if}
    <div class="term-input">
      <Button variant="ghost" size="icon" disabled={inputLocked} aria-label="Attach image" onclick={() => fileInput.click()}>
        <svg class="button-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
          <rect x="3" y="4" width="18" height="16" rx="2"></rect>
          <circle cx="8.5" cy="9" r="1.5"></circle>
          <path d="m4 17 4.5-4.5 3.5 3.5 2.5-2.5L20 19"></path>
        </svg>
      </Button>
      <div class:awaiting-approval={approvalMode && !composerFocused} class:has-text={Boolean(composer)} class="composer-field">
        <textarea
          bind:this={composerElement}
          bind:value={composer}
          rows="1"
          disabled={composerLocked}
          placeholder={approvalMode
            ? 'Approval pending — use buttons'
            : inspectionMode
              ? terminalTextMode
                ? 'Type terminal input…'
                : 'Needs inspection — use terminal controls'
              : 'Type a reply…'}
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
        {#if composer}<button class="input-clear" aria-label="Clear prompt text" onclick={clearComposer}>×</button>{/if}
      </div>
      <Button size="icon" disabled={!composer.replace(/[\r\n]+$/g, '') || composerLocked || sendingPrompt} aria-label={sendingPrompt ? 'Submitting input' : inspectionMode ? 'Submit terminal text' : 'Send prompt'} onclick={sendPrompt}>{sendingPrompt ? '…' : '➤'}</Button>
      <input bind:this={fileInput} type="file" accept="image/*" multiple hidden onchange={(event) => { void filesSelected(event.currentTarget.files || []); event.currentTarget.value = ''; }} />
    </div>
    {#if uploadStatus}<p class:error={uploadError} class="upload-status" role="status">{uploadStatus}</p>{/if}
    {#if draftPersistenceWarning}<p class="upload-status error" role="status">{draftPersistenceWarning}</p>{/if}
    {#if paneSizeLeaseError}<p class="upload-status error" role="alert">{paneSizeLeaseError}</p>{/if}
    {#if historyTruncated}
      <p class="upload-status" role="status">Older terminal history is not shown; this pane response was limited.</p>
    {/if}

    {#if visibleTerminalMenu}
      <section class="generic-menu-actions" aria-label={`Terminal menu: ${visibleTerminalMenu.title}`} aria-busy={keySending}>
        <header>
          <strong>{visibleTerminalMenu.title}</strong>
          <span>Detected from terminal key hints</span>
          <button type="button" aria-label="Dismiss detected menu actions" onclick={() => { dismissedMenuSignature = visibleTerminalMenu.signature; }}>×</button>
        </header>
        <div>
          {#each visibleTerminalMenu.actions as action (action.keys.join('+'))}
            <Button
              variant={action.cancel ? 'secondary' : 'default'}
              size="sm"
              disabled={keySending}
              onclick={() => { void sendKeys(action.keys, action.label); }}
            ><kbd>{menuKeyLabel(action.keys)}</kbd>{action.label}</Button>
          {/each}
        </div>
      </section>
    {/if}

    {#if approvalMode && !responding.has(agent.pane_id)}
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

    {#if keyControlStatus}
      <p class:error={keyFeedbackError} class="key-feedback" role="status" aria-live="polite">{keyControlStatus}</p>
    {/if}
    <div class="term-keys" aria-busy={keySending}>
      <Button variant="secondary" size="sm" onpointerdown={(event) => event.preventDefault()} onclick={() => sendTerminalKey('Escape', 'Cancelled prompt')}>Esc</Button>
      <Button variant="secondary" size="sm" aria-label="Tab" title="Send Tab" onpointerdown={(event) => event.preventDefault()} onclick={sendTab}>{@render tabIcon()}</Button>
      <div class="modifier-menu">
        <input
          id="modifier-key-input"
          class="modifier-key-input"
          bind:this={modifierInputElement}
          aria-label="Modifier shortcut character"
          autocomplete="off"
          autocapitalize="none"
          maxlength="1"
          spellcheck="false"
          oninput={modifierInput}
          onkeydown={modifierKeydown}
          onblur={modifierBlur}
        />
        <Button
          variant="secondary"
          size="sm"
          aria-controls="modifier-key-input"
          aria-pressed={shiftArmed}
          aria-label="Shift"
          title="Arm Shift; combine it with Ctrl or Alt"
          onpointerdown={(event) => event.preventDefault()}
          onclick={toggleShift}
        >{@render shiftIcon()}</Button>
        <Button
          variant="secondary"
          size="sm"
          aria-controls="modifier-key-input"
          aria-pressed={ctrlArmed}
          aria-label="Ctrl"
          title="Arm Ctrl; combine it with Shift or Alt"
          onpointerdown={(event) => event.preventDefault()}
          onclick={toggleCtrl}
        ><span class="key-caret">^</span></Button>
        <Button
          variant="secondary"
          size="sm"
          aria-controls="modifier-key-input"
          aria-pressed={altArmed}
          title="Arm Alt; combine it with Ctrl or Shift"
          onpointerdown={(event) => event.preventDefault()}
          onclick={toggleAlt}
        >Alt</Button>
      </div>
      <span class="spacer"></span>
      <div class="fkey-menu">
        <Button
          variant="secondary"
          size="sm"
          aria-label="Function keys"
          aria-expanded={fkeysOpen}
          onpointerdown={(event) => event.preventDefault()}
          onclick={() => { fkeysOpen = !fkeysOpen; arrowsOpen = false; }}
        >F keys</Button>
        {@render fkeyPopup()}
      </div>
      <div class="arrow-menu">
        <Button
          variant="secondary"
          size="sm"
          aria-label="Arrow keys"
          aria-expanded={arrowsOpen}
          onpointerdown={(event) => event.preventDefault()}
          onclick={() => { arrowsOpen = !arrowsOpen; fkeysOpen = false; }}
        >
          {@render arrowIcon()}
        </Button>
        {@render arrowPopup()}
      </div>
      <Button variant="secondary" size="sm" aria-label="Enter" onpointerdown={(event) => event.preventDefault()} onclick={() => sendTerminalKey('Enter')}>Enter</Button>
    </div>
  </div>
</div>
</main>
