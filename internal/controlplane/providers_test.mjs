import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {runInNewContext} from 'node:vm';
const context = {structuredClone};
runInNewContext(readFileSync(new URL('./dist/providers.js', import.meta.url), 'utf8'), context);
function form(overrides = {}, original = {}) {
  const values = {name: 'route', url: 'http://127.0.0.1:8000', format: 'openai', 'auth-mode': 'none', credential: 'ignored',
    fallback: '', translation: 'native', client: 'anthropic', upstream: 'openai-chat', model: '', 'max-tokens': '', project: '', ...overrides};
  return {originalProvider: original, open: false, focused: '', querySelector(selector) {
    const key = selector.slice(3);
    return {value: values[key], focus: () => { this.focused = key; }};
  }};
}
const read = row => JSON.parse(JSON.stringify(context.ToranaProviders.read(row)));
test('fallback editor rejects silent protocol skips and accepts explicit cross-API bridges', () => {
  const validate = context.ToranaProviders.validateFallbacks;
  assert.throws(() => validate({p:{format:'openai',fallback:['backup']},backup:{format:'anthropic'}}), /primary route/);
  assert.throws(() => validate({p:{format:'openai',fallback:['missing']}}), /existing/);
  assert.throws(() => validate({p:{format:'openai',bridge:{client:'openai-chat',upstream:'openai-chat'},fallback:['backup']},backup:{format:'anthropic',bridge:{client:'anthropic',upstream:'anthropic'}}}), /same harness API/);
  assert.doesNotThrow(() => validate({p:{format:'openai',bridge:{client:'openai-chat',upstream:'openai-chat'},fallback:['backup']},backup:{format:'anthropic',bridge:{client:'openai-chat',upstream:'anthropic'}}}));
});
test('provider edits preserve unrelated config and remove a bridge explicitly', () => {
  const original = {pricing: {'model': {input_usd_per_mtok: 1}}, cache: {enabled: true, nested: {value: 'preserve'}}, responses_compaction: {enabled: true}, bridge: {client: 'anthropic', upstream: 'openai-chat'}};
  const output = read(form({name: 'renamed', 'auth-mode': 'caller'}, original));
  assert.equal(output.name, 'renamed');
  assert.deepEqual(output.provider.pricing, original.pricing);
  assert.deepEqual(output.provider.cache, original.cache);
  assert.deepEqual(output.provider.responses_compaction, original.responses_compaction);
  assert.equal(output.provider.bridge, null);
  assert.deepEqual(output.provider.auth, {mode: 'caller'});
  assert.ok(original.bridge);
  const direct = context.ToranaProviders.read(form({}, original));
  direct.provider.cache.nested.value = 'edited';
  assert.equal(original.cache.nested.value, 'preserve');
});
test('bridge fields set the upstream family and retain ordered fallback targets', () => {
  const output = read(form({translation: 'bridge', client: 'openai-responses', upstream: 'anthropic', model: 'claude-haiku-4-5', 'max-tokens': '2048', 'auth-mode': 'credential', credential: 'anthropic-key', fallback: 'local, backup'})).provider;
  assert.equal(output.format, 'anthropic');
  assert.deepEqual(output.auth, {mode: 'credential', credential: 'anthropic-key'});
  assert.deepEqual(output.bridge, {client: 'openai-responses', upstream: 'anthropic', model: 'claude-haiku-4-5', max_tokens: 2048});
  assert.deepEqual(output.fallback, ['local', 'backup']);
});
test('cross-family caller auth is refused without silently changing authentication', () => {
  const row = form({translation: 'bridge', 'auth-mode': 'caller'});
  assert.throws(() => read(row), /stored credential/);
  assert.equal(row.open, true); assert.equal(row.focused, 'auth-mode');
});
test('Code Assist requires a project and unrelated protocol options are removed', () => {
  assert.throws(() => read(form({translation: 'bridge', upstream: 'gemini-codeassist'})), /project/);
  const output = read(form({translation: 'bridge', upstream: 'gemini-codeassist', project: 'my-project'}, {bridge: {max_tokens: 99, model: 'old'}})).provider;
  assert.deepEqual(output.bridge, {client: 'anthropic', upstream: 'gemini-codeassist', project: 'my-project'});
});
test('invalid inputs are actionable and do not silently omit provider entries', () => {
  for (const values of [{name: ''}, {url: ''}, {'auth-mode': 'credential', credential: ''},
    {translation: 'bridge', upstream: 'anthropic', 'max-tokens': '0'},
    {translation: 'bridge', upstream: 'anthropic', 'max-tokens': '1.5'}]) {
    assert.throws(() => read(form(values)));
  }
});
