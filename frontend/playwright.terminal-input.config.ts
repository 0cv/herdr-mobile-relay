import { defineConfig } from '@playwright/test';
import attention from './playwright.attention.config';

export default defineConfig({ ...attention, testMatch: 'terminal-input.spec.ts', timeout: 45_000 });
