import { readFile } from 'node:fs/promises';
import { parseAndroidAvdName } from './support/android';

const filename = process.argv.at(-1);
if (!filename || filename === process.argv[0]) throw new Error('missing emulator console response file');
process.stdout.write(parseAndroidAvdName(await readFile(filename, 'utf8')) || '');
