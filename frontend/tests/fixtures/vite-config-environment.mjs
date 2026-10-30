import { builtinEnvironments } from 'vitest/runtime';

// Node configuration imports need file URLs; retain the existing DOM setup.
export default {
  ...builtinEnvironments.jsdom,
  viteEnvironment: 'ssr',
};
