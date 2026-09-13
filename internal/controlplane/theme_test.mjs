import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { runInNewContext } from 'node:vm';
const source = readFileSync(new URL('./dist/theme.js', import.meta.url), 'utf8');
function setup({ absent = false, noLabel = false, blocked = false } = {}) {
  const events = {}, root = { dataset: {} }, label = { hidden: true };
  const select = { value: '', closest: () => noLabel ? null : label, addEventListener: (_, fn) => events.change = fn };
  const media = { matches: true, addEventListener: (_, fn) => events.media = fn };
  const storage = { getItem() { if (blocked) throw Error('blocked'); return 'light'; }, setItem() { if (blocked) throw Error('blocked'); } };
  runInNewContext(source, {
    localStorage: storage,
    window: { matchMedia: () => media, addEventListener: (name, fn) => events[name] = fn },
    document: { documentElement: root, getElementById: () => absent ? null : select, addEventListener: (name, fn) => events[name] = fn },
  });
  return { events, root, select, storage, media };
}
test('local preference survives foreign storage clears and follows local clears', () => {
  const app = setup(); app.events.DOMContentLoaded();
  assert.equal(app.root.dataset.theme, 'light');
  app.events.storage({ key: null, newValue: null, storageArea: {} });
  assert.equal(app.select.value, 'light');
  app.events.storage({ key: null, newValue: null, storageArea: app.storage });
  assert.equal(app.select.value, 'system');
  assert.equal(app.root.dataset.theme, 'dark');
  app.media.matches = false; app.events.media();
  assert.equal(app.root.dataset.theme, 'light');
});
test('missing markup and blocked storage are safe', () => {
  for (const options of [{ absent: true }, { noLabel: true }, { blocked: true }]) {
    const app = setup(options);
    assert.doesNotThrow(() => app.events.DOMContentLoaded());
    if (!options.absent) { app.select.value = 'dark'; app.events.change(); assert.equal(app.root.dataset.theme, 'dark'); }
  }
});
