import { mount } from 'svelte';
import App from './App.svelte';
import { reportUncaughtErrors } from './lib/crash-banner';
import './app.css';
reportUncaughtErrors();

const target = document.getElementById('app');
if (!target) throw new Error('Missing #app mount point');

mount(App, { target });
