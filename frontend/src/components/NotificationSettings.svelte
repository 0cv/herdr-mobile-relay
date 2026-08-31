<script lang="ts">
  import AppSwitch from '$components/ui/AppSwitch.svelte';
  import Button from '$components/ui/Button.svelte';
  import Card from '$components/ui/Card.svelte';
  import {
    clearSnooze,
    pushPolicyScopeKey,
    withGlobalSnooze,
    withTimedSnooze,
    type DevicePushPolicy,
    type NotificationPlatformInfo,
    type NotificationPolicyScope,
    type PushCategory,
    type PushPolicyChange,
    type PushTestRequest,
    type PushTestState,
  } from '$lib/push-policy';

  type ConfigurableCategory = Exclude<PushCategory, 'brief' | 'update'>;
  const CATEGORY_LABELS: Record<ConfigurableCategory, { label: string; detail: string }> = {
    attention: { label: 'Approval needed', detail: 'An agent needs your review.' },
    question: { label: 'Questions', detail: 'An agent needs your answer.' },
    finished: { label: 'Finished', detail: 'An agent finished.' },
    test: { label: 'Test notifications', detail: 'Delivery checks for this device.' },
  };
  const CONFIGURABLE_CATEGORIES: ConfigurableCategory[] = ['attention', 'question', 'finished', 'test'];

  let {
    scopes = [],
    platform,
    testState = { status: 'idle' },
    testStates = {},
    busy = false,
    onpolicychange,
    onrequestpermission,
    ontest,
  }: {
    scopes?: NotificationPolicyScope[];
    platform: NotificationPlatformInfo;
    testState?: PushTestState;
    testStates?: Record<string, PushTestState>;
    busy?: boolean;
    onpolicychange?: (change: PushPolicyChange) => void | Promise<void>;
    onrequestpermission?: () => void | Promise<void>;
    ontest?: (request: PushTestRequest) => void | Promise<void>;
  } = $props();

  let selectedKey = $state('');
  let localPolicies = $state<Record<string, DevicePushPolicy>>({});
  let saving = $state(false);
  let policyError = $state('');

  const selectedScope = $derived.by(() => {
    const exact = scopes.find(scope => pushPolicyScopeKey(scope.relay_id, scope.device_id) === selectedKey);
    return exact || scopes[0] || null;
  });
  const effectiveSelectedKey = $derived(selectedScope
    ? pushPolicyScopeKey(selectedScope.relay_id, selectedScope.device_id)
    : '');
  const selectedPolicy = $derived(selectedScope
    ? localPolicies[effectiveSelectedKey] || selectedScope.policy
    : null);
  const selectedTestState = $derived(selectedScope
    ? testStates[selectedScope.relay_id] || testState
    : testState);

  function chooseScope(event: Event): void {
    selectedKey = (event.currentTarget as HTMLSelectElement).value;
  }

  async function applyPolicy(policy: DevicePushPolicy): Promise<void> {
    if (!selectedScope || !selectedPolicy || saving) return;
    const key = effectiveSelectedKey;
    const previous = selectedPolicy;
    localPolicies = { ...localPolicies, [key]: policy };
    policyError = '';
    saving = true;
    try {
      await onpolicychange?.({
        relay_id: selectedScope.relay_id,
        device_id: selectedScope.device_id,
        policy,
      });
    } catch {
      localPolicies = { ...localPolicies, [key]: previous };
      policyError = 'The relay did not save this notification policy.';
    } finally {
      saving = false;
    }
  }

  function changeCategory(category: PushCategory, checked: boolean): void {
    if (!selectedPolicy) return;
    applyPolicy({
      ...selectedPolicy,
      categories: { ...selectedPolicy.categories, [category]: checked },
    });
  }

  function changeMilliseconds(field: 'settle_ms' | 'cooldown_ms', event: Event): void {
    if (!selectedPolicy) return;
    const value = Number((event.currentTarget as HTMLSelectElement).value);
    applyPolicy({ ...selectedPolicy, [field]: value });
  }

  function changeSnooze(event: Event): void {
    if (!selectedPolicy) return;
    const duration = (event.currentTarget as HTMLSelectElement).value;
    if (duration === 'global') applyPolicy(withGlobalSnooze(selectedPolicy));
    else if (duration === 'off') applyPolicy(clearSnooze(selectedPolicy));
    else applyPolicy(withTimedSnooze(selectedPolicy, Number(duration)));
  }

  function snoozeSelection(policy: DevicePushPolicy): string {
    if (!policy.snoozed) return 'off';
    return policy.snooze_until ? 'timed' : 'global';
  }

  function sendTest(): void {
    if (!selectedScope?.current_device) return;
    void ontest?.({ relay_id: selectedScope.relay_id });
  }

  const testMessage = $derived.by(() => {
    if (selectedTestState.status === 'sending') return 'Asking the relay service to send a neutral test…';
    if (selectedTestState.status === 'accepted') {
      const verb = selectedTestState.result === 'queued' ? 'queued' : 'accepted';
      return `The relay service ${verb} the test. This does not confirm that the phone displayed it.`;
    }
    if (selectedTestState.status === 'rejected') return `The relay could not accept the test (${selectedTestState.code}).`;
    return 'A test reports relay service acceptance only; it cannot confirm handset display or human delivery.';
  });
</script>

<div class="notification-settings">
  <Card aria-labelledby="notification-settings-title">
    <h2 id="notification-settings-title">Notifications</h2>
    <p class="intro">Set notifications per relay and device.</p>
    {#if scopes.length > 0 && selectedScope && selectedPolicy}
      <label class="field-label" for="notification-scope">Relay and device</label>
      <select id="notification-scope" value={effectiveSelectedKey} onchange={chooseScope} disabled={busy || saving}>
        {#each scopes as scope (pushPolicyScopeKey(scope.relay_id, scope.device_id))}
          <option value={pushPolicyScopeKey(scope.relay_id, scope.device_id)}>
            {scope.relay_label} — {scope.device_label}{scope.current_device ? ' (this device)' : ''}
          </option>
        {/each}
      </select>

      <fieldset disabled={busy || saving}>
        <legend>Categories</legend>
        {#each CONFIGURABLE_CATEGORIES as category (category)}
          <div class="setting-row">
            <AppSwitch
              checked={selectedPolicy.categories[category]}
              label={CATEGORY_LABELS[category].label}
              descriptionId={`push-category-${category}`}
              onchange={checked => changeCategory(category, checked)}
            />
            <p id={`push-category-${category}`} class="detail">{CATEGORY_LABELS[category].detail}</p>
          </div>
        {/each}
      </fieldset>

      <div class="grid">
        <label>
          <span>Settle delay</span>
          <select value={String(selectedPolicy.settle_ms)} onchange={event => changeMilliseconds('settle_ms', event)} disabled={busy || saving}>
            <option value="0">Immediately</option>
            <option value="2000">2 seconds</option>
            <option value="5000">5 seconds</option>
            <option value="15000">15 seconds</option>
          </select>
        </label>
        <label>
          <span>Cooldown</span>
          <select value={String(selectedPolicy.cooldown_ms)} onchange={event => changeMilliseconds('cooldown_ms', event)} disabled={busy || saving}>
            <option value="0">None</option>
            <option value="30000">30 seconds</option>
            <option value="60000">1 minute</option>
            <option value="300000">5 minutes</option>
          </select>
        </label>
        <label>
          <span>Snooze</span>
          <select value={snoozeSelection(selectedPolicy)} onchange={changeSnooze} disabled={busy || saving}>
            <option value="off">Not snoozed</option>
            {#if selectedPolicy.snoozed && selectedPolicy.snooze_until}<option value="timed">Until {new Date(selectedPolicy.snooze_until).toLocaleString()}</option>{/if}
            <option value="3600000">For 1 hour</option>
            <option value="28800000">For 8 hours</option>
            <option value="86400000">For 24 hours</option>
            <option value="global">Until I turn it back on</option>
          </select>
        </label>
      </div>

      {#if policyError}<p class="policy-error" role="alert">{policyError}</p>{/if}

      <div class="test-row">
        <Button
          variant="secondary"
          disabled={busy || saving || selectedTestState.status === 'sending' || !selectedScope.current_device}
          onclick={sendTest}
        >Send neutral test</Button>
        {#if !selectedScope.current_device}
          <p class="detail">Test notifications from that device.</p>
        {/if}
      </div>
      <p class="test-result" aria-live="polite">{testMessage}</p>
    {:else}
      <p class="empty">No paired notification device is available.</p>
    {/if}
  </Card>

  <Card aria-labelledby="notification-help-title">
    <h2 id="notification-help-title">Permission and installation</h2>
    {#if !platform.supports_push}
      <p role="status">Push is unavailable here.</p>
    {:else if platform.permission === 'granted'}
      <p class="success" role="status">Notifications are allowed.</p>
    {:else if platform.permission === 'denied'}
      <p role="status">Notifications are blocked. Enable them in system settings below.</p>
    {:else}
      <Button disabled={busy} onclick={() => void onrequestpermission?.()}>Allow notifications</Button>
      <p class="detail">Your browser asks after you press the button.</p>
    {/if}

    {#if platform.platform === 'ios' && !platform.installed}
      <section class="guidance" aria-labelledby="ios-install-title">
        <h3 id="ios-install-title">Install on iPhone or iPad first</h3>
        <ol>
          <li>Open this site in Safari on iOS or iPadOS 16.4+.</li>
          <li>Tap Share, Add to Home Screen, then Add.</li>
          <li>Open Herdr from the Home Screen and return here.</li>
        </ol>
        <p>iOS allows Web Push only from an installed Home Screen app after a tap.</p>
      </section>
    {/if}

    <section class="guidance" aria-labelledby="manual-settings-title">
      <h3 id="manual-settings-title">Manual notification settings</h3>
      {#if platform.platform === 'ios'}
        <p><strong>iPhone or iPad:</strong> In Settings, open Notifications, choose Herdr, and enable Allow Notifications.</p>
      {:else if platform.platform === 'android'}
        <p><strong>Android:</strong> Long-press Herdr, open App info, then Notifications. For a browser tab, use its site settings.</p>
      {:else}
        <p>Use your browser’s site permissions and your system’s notification settings.</p>
      {/if}
    </section>
  </Card>
</div>

<style>
  .notification-settings { display: grid; gap: 1rem; }
  h2, h3, p { margin-top: 0; }
  h2 { font-size: 1rem; margin-bottom: .35rem; }
  h3 { font-size: .92rem; margin-bottom: .4rem; }
  .intro, .detail, .empty { color: var(--muted); font-size: .82rem; }
  .field-label, label > span { display: block; font-size: .78rem; font-weight: 650; margin-bottom: .3rem; }
  select { width: 100%; min-height: 2.4rem; border: 1px solid var(--border); border-radius: .5rem; background: var(--input); color: var(--foreground); padding: .45rem .6rem; }
  fieldset { border: 0; margin: 1rem 0; padding: 0; }
  legend { font-size: .82rem; font-weight: 700; margin-bottom: .5rem; }
  .setting-row { border-top: 1px solid var(--border); padding: .7rem 0 .45rem; }
  .setting-row .detail { margin: .25rem 0 0; }
  .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(9rem, 1fr)); gap: .7rem; }
  .test-row { display: flex; align-items: center; gap: .7rem; margin-top: .9rem; }
  .test-row .detail { margin: 0; }
  .test-result { min-height: 2.5em; margin: .5rem 0 0; color: var(--muted); font-size: .8rem; }
  .success { color: var(--success); }
  .guidance { border-top: 1px solid var(--border); margin-top: .9rem; padding-top: .9rem; }
  .guidance p, .guidance li { font-size: .84rem; line-height: 1.45; }
  .guidance ol { margin: .35rem 0 .65rem; padding-left: 1.35rem; }
</style>
