// Share geometry and color calculations without coupling them to shell sessions.
export function terminalHeightBounds(available) {
  const min = Math.min(140, Math.max(72, available - 100));
  return { min, max: Math.max(min, available - 140) };
}
export function terminalHeight(preferred, available) {
  const bounds = terminalHeightBounds(available);
  return Math.round(Math.max(bounds.min, Math.min(bounds.max, Number.isFinite(preferred) ? preferred : 260)));
}
export function terminalThemeColors(tokens) {
  const background = (tokens.bg || '#1e1e2e').trim();
  const rgb = /^#([\da-f]{6})$/i.exec(background);
  const channels = rgb ? rgb[1].match(/../g).map(c => parseInt(c, 16)) : /^rgb\(\s*(\d+)[,\s]+(\d+)[,\s]+(\d+)/i.exec(background)?.slice(1).map(Number);
  const grey = channels ? '#' + channels.map(c => Math.round(c * .88 + 128 * .12).toString(16).padStart(2, '0')).join('') : background;
  return {
    background: grey, foreground: tokens.fg, cursor: tokens.fg, cursorAccent: grey,
    selectionBackground: tokens.selection, selectionInactiveBackground: tokens.selection,
    black: tokens.bg, brightBlack: tokens.faint,
    red: tokens.red, brightRed: tokens.red, green: tokens.green, brightGreen: tokens.green,
    yellow: tokens.yellow, brightYellow: tokens.yellow, blue: tokens.blue, brightBlue: tokens.blue,
    magenta: tokens.magenta, brightMagenta: tokens.magenta, cyan: tokens.cyan, brightCyan: tokens.cyan,
    white: tokens.dim, brightWhite: tokens.fg,
  };
}
