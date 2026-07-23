<script lang="ts">
  let {
    checked = $bindable(false),
    disabled = false,
    label,
    descriptionId,
    onchange,
  }: {
    checked?: boolean;
    disabled?: boolean;
    label: string;
    descriptionId?: string;
    onchange?: (checked: boolean) => void;
  } = $props();

  function changed(event: Event) {
    checked = (event.currentTarget as HTMLInputElement).checked;
    onchange?.(checked);
  }
</script>

<label class="switch-row">
  <span>{label}</span>
  <input
    class="switch-root"
    type="checkbox"
    role="switch"
    {checked}
    {disabled}
    aria-describedby={descriptionId}
    onchange={changed}
  />
</label>
