// Incremental framing and awaited writes bound memory when a terminal produces bursts.
export async function readTerminalFrames(reader, consume) {
  const decoder = new TextDecoder(); let pending = '';
  try {
    while (true) {
      const { value, done } = await reader.read();
      pending += decoder.decode(value, { stream: !done });
      if (pending.length > 131072) throw new Error('Terminal stream frame is too large');
      let end;
      while ((end = pending.indexOf('\n')) >= 0) {
        const line = pending.slice(0, end); pending = pending.slice(end + 1);
        if (line) await consume(JSON.parse(line));
      }
      if (done) { if (pending.trim()) throw new Error('Terminal stream ended unexpectedly'); return; }
    }
  } finally { await reader.cancel().catch(() => {}); reader.releaseLock(); }
}
export function terminalBytesBase64(bytes) {
  let text = ''; for (const byte of bytes) text += String.fromCharCode(byte); return btoa(text);
}
