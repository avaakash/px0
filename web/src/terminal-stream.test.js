import test from 'node:test';
import assert from 'node:assert/strict';
import { readTerminalFrames, terminalBytesBase64 } from './terminal-stream.js';

test('terminal frames tolerate split network chunks and await rendering before reading more', async () => {
  const chunks = ['{"data":"Y', 'WJj","offset":3}\n{}\n{"exited":true,', '"code":7}\n'];
  const events = []; let reading = false;
  const reader = new ReadableStream({ pull(controller) { const chunk = chunks.shift(); if (chunk === undefined) controller.close(); else controller.enqueue(new TextEncoder().encode(chunk)); } }).getReader();
  await readTerminalFrames(reader, async frame => { assert.equal(reading, false); reading = true; await Promise.resolve(); events.push(frame); reading = false; });
  assert.deepEqual(events, [{ data: 'YWJj', offset: 3 }, {}, { exited: true, code: 7 }]);
});
test('terminal stream rejects unbounded/malformed data and preserves binary input', async () => {
  const reader = new ReadableStream({ start(c) { c.enqueue(new TextEncoder().encode('x'.repeat(131073))); c.close(); } }).getReader();
  await assert.rejects(readTerminalFrames(reader, () => {}), /too large/);
  assert.equal(terminalBytesBase64(Uint8Array.from([0, 3, 27, 128, 255])), 'AAMbgP8=');
});
