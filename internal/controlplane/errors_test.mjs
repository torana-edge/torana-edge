import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {runInNewContext} from 'node:vm';
const context = {};
runInNewContext(readFileSync(new URL('./dist/errors.js', import.meta.url), 'utf8'), context);
const message = context.ToranaErrors.message;
test('stale writes explain how to recover without replaying the write', () => {
  assert.match(message(JSON.stringify({error: {code:'stale_revision', message:'old'}}), 409), /Reload the page, review/);
});
test('structured errors display their message, not JSON', () => {
  assert.equal(message('{"error":{"code":"invalid","message":"Choose a provider"}}', 400), 'Choose a provider');
});
test('plain text survives and HTML or empty errors get a status fallback', () => {
  assert.equal(message('Connection closed', 502), 'Connection closed');
  assert.match(message('<html>proxy error</html>', 502), /HTTP 502/);
  assert.match(message('', 503), /HTTP 503/);
});
