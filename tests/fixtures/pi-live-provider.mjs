import { writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';

export default async function (pi) {
  const work = process.env.PI_LIVE_TEST_WORK;
  if (!work || !process.env.PI_TEST_PACKAGE || process.env.PI_OFFLINE !== '1') throw new Error('Private offline test environment required');
  const { createAssistantMessageEventStream } = await import(pathToFileURL(resolve(process.env.PI_TEST_PACKAGE, '../pi-ai/dist/index.js')));
  pi.registerProvider('herdr-native-fixture', {
    api: 'herdr-native-fixture', baseUrl: 'http://invalid.invalid', apiKey: 'synthetic-not-a-credential',
    models: [{ id: 'synthetic', name: 'Synthetic native fixture', reasoning: false, input: ['text'], contextWindow: 100000, maxTokens: 1024, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } }],
    streamSimple(model) {
      const stream = createAssistantMessageEventStream();
      void (async () => {
        await new Promise(resolve => setTimeout(resolve, process.env.PI_TEST_APP_FIXTURE ? 8000 : 1500));
        const message = { role: 'assistant', content: [{ type: 'text', text: '' }], api: model.api, provider: model.provider, model: model.id, timestamp: Date.now(), stopReason: 'pending', usage: { input: 1, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 2, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } } };
        stream.push({ type: 'start', partial: structuredClone(message) });
        stream.push({ type: 'text_start', contentIndex: 0, partial: structuredClone(message) });
        for (let index = 0; index < 40; index++) {
          const delta = `Visible fragment ${index}. `;
          message.content[0].text += delta;
          stream.push({ type: 'text_delta', contentIndex: 0, delta, partial: structuredClone(message) });
          await new Promise(resolve => setTimeout(resolve, 40));
        }
        stream.push({ type: 'text_end', contentIndex: 0, content: message.content[0].text, partial: structuredClone(message) });
        message.stopReason = 'stop';
        stream.push({ type: 'done', reason: 'stop', message });
        stream.end();
      })();
      return stream;
    },
  });
  pi.on('input', async event => {
    await writeFile(join(work, 'desktop-input.json'), JSON.stringify({ accepted: event.text === 'Synthetic private qualification prompt', source: event.source }), { mode: 0o600 });
  });
  pi.on('session_start', async (_event, ctx) => {
    await writeFile(join(work, 'identity.json'), JSON.stringify({ pid: process.pid, session: ctx.sessionManager.getSessionId(), mode: ctx.mode }) + '\n', { mode: 0o600 });
  });
  pi.on('agent_settled', (_event, ctx) => { setTimeout(() => ctx.shutdown(), process.env.PI_TEST_APP_FIXTURE ? 4000 : 1500); });
}
