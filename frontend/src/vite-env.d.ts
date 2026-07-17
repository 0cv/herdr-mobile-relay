/// <reference types="vite/client" />

import 'svelte/elements';

declare module 'svelte/elements' {
  interface HTMLTextareaAttributes {
    autocorrect?: 'on' | 'off';
  }
}

declare global {
  const HERDR_NOTIFICATION_ICON: string;
  const HERDR_NOTIFICATION_BADGE: string;

  interface NotificationAction {
    action: string;
    title: string;
    icon?: string;
  }

  interface NotificationOptions {
    actions?: NotificationAction[];
    renotify?: boolean;
  }

  interface Navigator {
    clearAppBadge?: () => Promise<void>;
    setAppBadge?: (contents?: number) => Promise<void>;
    standalone?: boolean;
  }
}

export {};
