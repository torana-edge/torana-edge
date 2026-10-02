import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {runInNewContext} from 'node:vm';

class Element {
  constructor(tag) { this.tag = tag; this.children = []; this.listeners = {}; this.textContent = ''; }
  append(...nodes) { this.children.push(...nodes); }
  replaceChildren(...nodes) { this.children = nodes; }
  addEventListener(name, callback) { this.listeners[name] = callback; }
  setAttribute(name, value) { this[name] = value; }
  removeAttribute(name) { delete this[name]; }
  set innerHTML(_) { throw new Error('Approval metadata must never become HTML'); }
}
const reference = 'tr_' + 'a'.repeat(64);
const record = {reference, plugin: 'pii', status: 'pending', call_id: 'call-a', conversation: 'session-a'};
const text = el => el.textContent + el.children.map(text).join(' ');
const all = (el, tag) => [...(el.tag === tag ? [el] : []), ...el.children.flatMap(child => all(child, tag))];
function setup(request) {
  const elements = {resultApprovals: new Element('section'), moreResultApprovals: new Element('button')};
  const alerts = [];
  const context = {document: {createElement: tag => new Element(tag), getElementById: id => elements[id]}, ToranaConsent: {request}, showAlert: message => alerts.push(message)};
  runInNewContext(readFileSync(new URL('./dist/result-approvals.js', import.meta.url), 'utf8'), context);
  return {api: context.ToranaResultApprovals, elements, alerts};
}

test('allowance requires explicit human acknowledgement and uses reviewed status', async () => {
  const calls = [];
  let finish;
  const {api, elements} = setup(async (path, input) => {
    calls.push({path, input});
    if (!input) return {approvals: [record]};
    return new Promise(resolve => { finish = resolve; });
  });
  await api.load();
  const [allow, decline] = all(elements.resultApprovals, 'button');
  const [checkbox] = all(elements.resultApprovals, 'input');
  assert.equal(allow.disabled, true);
  await allow.listeners.click();
  assert.equal(calls.length, 1);
  checkbox.checked = true;
  checkbox.listeners.change();
  assert.equal(allow.disabled, false);
  const saving = allow.listeners.click();
  assert.equal(allow.disabled, true);
  assert.equal(decline.disabled, true);
  assert.equal(checkbox.disabled, true);
  assert.equal(calls[1].path, `/_torana/api/v1/approvals/${reference}/approve`);
  assert.deepEqual(JSON.parse(JSON.stringify(calls[1].input)), {expected_status: 'pending'});
  finish({...record, status: 'approved'});
  await saving;
  assert.match(text(elements.resultApprovals), /approved.*Revoke allowance/);
  assert.doesNotMatch(text(elements.resultApprovals), /Allow upstream/);
});

test('failed decisions keep pending state and restore controls without fake success', async () => {
  const {api, elements, alerts} = setup(async (_, input) => {
    if (input) throw new Error('This approval changed; reload it.');
    return {approvals: [record]};
  });
  await api.load();
  const [allow, decline] = all(elements.resultApprovals, 'button');
  await decline.listeners.click();
  assert.equal(allow.disabled, true);
  assert.equal(decline.disabled, false);
  assert.match(alerts[0], /approval changed/);
  assert.match(text(elements.resultApprovals), /pending/);
});

test('metadata is text and pagination retains the first page', async () => {
  const calls = [];
  const {api, elements} = setup(async path => {
    calls.push(path);
    return calls.length === 1 ? {approvals: [{...record, plugin: '<img onerror=alert(1)>'}], next_cursor: 'a+b/c'}
      : {approvals: [{...record, reference: 'tr_' + 'b'.repeat(64), status: 'revoked'}]};
  });
  await api.load();
  assert.equal(elements.moreResultApprovals.hidden, false);
  await api.loadMore();
  assert.equal(calls[1], '/_torana/api/v1/approvals?cursor=a%2Bb%2Fc');
  assert.match(text(elements.resultApprovals), /<img onerror=alert\(1\)>/);
  assert.match(text(elements.resultApprovals), /revoked/);
  assert.equal(elements.moreResultApprovals.hidden, true);
  assert.equal(api.referenceOK('../other'), false);
});

test('a slow reload cannot overwrite the latest result list', async () => {
  const pending = [];
  const {api, elements} = setup(() => new Promise(resolve => pending.push(resolve)));
  const first = api.load();
  const second = api.load();
  pending[1]({approvals: [{...record, status: 'approved'}]});
  await second;
  pending[0]({approvals: [record]});
  await first;
  assert.match(text(elements.resultApprovals), /approved/);
  assert.doesNotMatch(text(elements.resultApprovals), /Allow upstream/);
});
