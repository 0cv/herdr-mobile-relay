import { createQuestionDraftCache as createCache } from '../lib/questions';

export * from '../lib/questions';

const cache = createCache();

export const createQuestionDraftCache = () => cache;
