import js from '@eslint/js';
import globals from 'globals';
import tseslint from 'typescript-eslint';

export default tseslint.config(
  { ignores: ['run-artifacts/', 'node_modules/'] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    files: ['android-appium/*.cjs', 'unit/android-*.cjs', 'unit/android-*.mjs'],
    languageOptions: { globals: globals.node },
    rules: {
      '@typescript-eslint/no-require-imports': 'off',
      '@typescript-eslint/no-unused-vars': ['error', { argsIgnorePattern: '^_' }],
    },
  },
  {
    files: ['**/*.ts'],
    languageOptions: {
      globals: {
        ...globals.node,
        ...globals.browser,
      },
      parserOptions: {
        project: './tsconfig.json',
      },
    },
    rules: {
      '@typescript-eslint/no-explicit-any': 'off',
      '@typescript-eslint/no-unused-vars': ['error', { argsIgnorePattern: '^_' }],
    },
  },
);
