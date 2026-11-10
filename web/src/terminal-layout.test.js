import test from 'node:test';
import assert from 'node:assert/strict';
import { terminalHeight, terminalHeightBounds, terminalThemeColors } from './terminal-layout.js';

test('saved terminal height leaves editor space and adapts to shorter windows', () => {
  assert.equal(terminalHeight(320, 900), 320);
  const short = terminalHeight(320, 380);
  assert.ok(short < 320 && 380 - short >= 140);
  assert.equal(terminalHeight(320, 900), 320, 'resizing the window must not replace the saved preference');
  for (const height of [180, 320, 720, 1200]) {
    const { min, max } = terminalHeightBounds(height);
    assert.ok(min > 0 && min <= max && max < height);
    assert.equal(terminalHeight(-100, height), min);
    assert.equal(terminalHeight(Infinity, height), terminalHeight(260, height));
  }
});

test('terminal colors follow light and dark palettes with a modest neutral tint', () => {
  const dark = terminalThemeColors({ bg: '#1e1e2e', fg: '#cdd6f4', red: '#f38ba8', selection: 'rgba(147,153,178,.25)' });
  const light = terminalThemeColors({ bg: '#ffffff', fg: '#24292f', red: '#cf222e', selection: '#b6e3ff' });
  assert.equal(dark.foreground, '#cdd6f4'); assert.equal(light.foreground, '#24292f');
  assert.equal(dark.cursor, dark.foreground); assert.equal(light.cursorAccent, light.background);
  assert.equal(dark.selectionBackground, 'rgba(147,153,178,.25)'); assert.equal(light.red, '#cf222e');
  const channel = color => parseInt(color.slice(1, 3), 16);
  assert.ok(channel(dark.background) > 30 && channel(dark.background) < 60);
  assert.ok(channel(light.background) > 230 && channel(light.background) < 255);
  assert.equal(terminalThemeColors({ bg: 'rgb(30, 30, 46)' }).background, dark.background);
});
