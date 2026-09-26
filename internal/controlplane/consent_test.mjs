import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {runInNewContext} from 'node:vm';

class Element {
  constructor(tag) { this.tag = tag; this.children = []; this.listeners = {}; this.style = {}; this.textContent = ''; }
  append(...nodes) { this.children.push(...nodes); }
  replaceChildren(...nodes) { this.children = nodes; }
  addEventListener(name, callback) { this.listeners[name] = callback; }
  set innerHTML(_) { throw new Error('Untrusted content must never use HTML'); }
}
function setup(fetch = async () => ({ok: true, json: async () => ({})})) {
  const elements = Object.fromEntries(['reviewConversation', 'reviewConversations', 'reviewSuggestions', 'reviewChanges'].map(id => [id, new Element('div')]));
  elements.reviewConversation.value = '';
  const context = {fetch, document: {createElement: tag => new Element(tag), getElementById: id => elements[id]}, showAlert() {}};
  runInNewContext(readFileSync(new URL('./dist/consent.js', import.meta.url), 'utf8'), context);
  return {api: context.ToranaConsent, elements};
}
const text = element => element.textContent + element.children.map(text).join(' ');

test('review actions are scoped, encoded and carry the local mutation marker', async () => {
  const {api} = setup();
  const calls = [];
  await api.request(api.actionPath('changes', 'sg:change', 'undo'), {conversation_id: 'conversation'}, async (path, options) => {
    calls.push({path, options});
    return {ok: true, json: async () => ({ok: true, status: 'undone'})};
  });
  assert.equal(calls[0].path, '/_torana/api/v1/agent/changes/sg%3Achange/undo');
  assert.equal(calls[0].options.method, 'POST');
  assert.equal(calls[0].options.headers['X-Torana-Local-Request'], '1');
  assert.deepEqual(JSON.parse(calls[0].options.body), {conversation_id: 'conversation'});
  assert.throws(() => api.actionPath('changes', '../other', 'undo'), /Invalid/);
  assert.throws(() => api.actionPath('changes', 'sg_1', 'accept'), /Invalid/);
});

test('execution refusals do not look like successful acceptance', async () => {
  const {api} = setup();
  for (const data of [{execution: {ok: false, error: {message: 'Configuration changed.'}}}, {ok: false}]) {
    await assert.rejects(api.request('/unused', {}, async () => ({ok: true, json: async () => data})), /Configuration changed|did not complete/);
  }
});

test('review renders untrusted text, never codes or executable HTML', () => {
  const {api} = setup();
  const container = new Element('section');
  api.render(container, [{id: 'sg_1', code: 'private-code', title: '<img onerror=alert(1)>', body: 'Before → after', status: 'pending'}], 'conversation', 'suggestions');
  assert.match(text(container), /<img onerror=alert\(1\)>/);
  assert.match(text(container), /Before → after/);
  assert.doesNotMatch(text(container), /private-code/);
  assert.match(text(container), /Accept.*Dismiss/);
});

test('late results cannot replace the newly selected conversation', async () => {
  const pending = [];
  const {api, elements} = setup(path => new Promise(resolve => pending.push({path, resolve})));
  elements.reviewConversation.value = 'first';
  const first = api.loadSelected();
  elements.reviewConversation.value = 'second';
  const second = api.loadSelected();
  for (const {path, resolve} of pending.filter(item => item.path.includes('second'))) {
    resolve({ok: true, json: async () => path.includes('suggestions') ? {suggestions: [{id: 'sg_2', title: 'Second conversation', status: 'pending'}]} : {changes: []}});
  }
  await second;
  for (const {path, resolve} of pending.filter(item => item.path.includes('first'))) {
    resolve({ok: true, json: async () => path.includes('suggestions') ? {suggestions: [{id: 'sg_1', title: 'First conversation', status: 'pending'}]} : {changes: []}});
  }
  await first;
  assert.match(text(elements.reviewSuggestions), /Second conversation/);
  assert.doesNotMatch(text(elements.reviewSuggestions), /First conversation/);
});
