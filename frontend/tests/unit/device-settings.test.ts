import { render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import DeviceSettings from '$components/DeviceSettings.svelte';

const device = {
  deviceId: 'device-current',
  credentialId: 'credential-current',
  name: 'Current phone',
  role: 'controller' as const,
  pairedAt: Date.now(),
  current: true,
};

describe('device settings', () => {
  it('creates and exposes a one-use invitation link', async () => {
    const user = userEvent.setup();
    const invitationLink = 'https://app.example/#setup=secret&invite=invitation';
    const onInvite = vi.fn().mockResolvedValue(invitationLink);
    render(DeviceSettings, {
      relayId: 'relay-a',
      relayLabel: 'Laptop',
      devices: [device],
      currentDeviceId: device.deviceId,
      connected: true,
      canAdminister: true,
      onRename: vi.fn().mockResolvedValue(undefined),
      onInvite,
      onRevoke: vi.fn().mockResolvedValue(undefined),
      onReset: vi.fn().mockResolvedValue(undefined),
      onForgetCurrent: vi.fn().mockResolvedValue(undefined),
    });

    await user.click(screen.getByRole('button', { name: 'Invite Device' }));
    await user.type(screen.getByRole('textbox', { name: 'Device name' }), 'Review tablet');
    await user.click(screen.getByRole('button', { name: 'Create Invitation' }));

    expect(onInvite).toHaveBeenCalledWith({ relayId: 'relay-a', name: 'Review tablet', role: 'reader' });
    expect(await screen.findByDisplayValue(invitationLink)).toHaveAttribute('readonly');
    expect(screen.getByRole('status')).toHaveTextContent('Share the one-use link');
  });
});
