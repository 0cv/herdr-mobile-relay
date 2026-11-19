import { mount } from 'svelte';
import DemoHome from './DemoHome.svelte';

const target = document.getElementById('app');
if (!target) throw new Error('Missing #app mount point');

mount(DemoHome, { target });
