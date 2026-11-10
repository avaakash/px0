export const escapeWorktreeText = value => String(value ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

// These rows intentionally have no editor line targets or mutation controls:
// their paths belong to the selected checkout, not the open editor workspace.
export function worktreeDiffHTML(hunks, mode = 'split', limit = 2000) {
  let html = '', count = 0, total = 0;
  const e = escapeWorktreeText;
  const side = (row, which) => {
    const type = ['add', 'del', 'ctx'].includes(row?.type) ? row.type : 'blank';
    const n = row && (which === 'left' ? row.oldLine : row.newLine);
    return '<div class="diff-side diff-' + type + '"><span class="diff-ln">' + (Number.isInteger(n) ? n : '') + '</span><span class="diff-mk">' + ({ add: '+', del: '−' }[type] || '') + '</span><span class="diff-code">' + (row?.html || e(row?.text) || '&nbsp;') + '</span></div>';
  };
  for (const h of hunks || []) {
    total += h.rows.length;
    if (count >= limit) continue;
    html += '<div class="diff-hunk-head">@@ −' + e(h.oldStart) + ' +' + e(h.newStart) + ' @@ ' + e(h.section) + '</div><div class="diff-table diff-' + mode + '">';
    const rows = h.rows.slice(0, limit - count);
    count += rows.length;
    if (mode === 'unified') {
      for (const row of rows) {
        const type = ['add', 'del', 'ctx'].includes(row.type) ? row.type : 'ctx';
        html += '<div class="diff-row diff-' + type + '"><span class="diff-ln">' + (Number.isInteger(row.oldLine) ? row.oldLine : '') + '</span><span class="diff-ln">' + (Number.isInteger(row.newLine) ? row.newLine : '') + '</span><span class="diff-mk">' + ({ add: '+', del: '−' }[type] || '') + '</span><span class="diff-code">' + (row.html || e(row.text) || '&nbsp;') + '</span></div>';
      }
    } else {
      for (let i = 0; i < rows.length;) {
        if (rows[i].type === 'ctx') { html += '<div class="diff-row-pair">' + side(rows[i], 'left') + side(rows[i++], 'right') + '</div>'; continue; }
        const dels = [], adds = [];
        while (i < rows.length && rows[i].type === 'del') dels.push(rows[i++]);
        while (i < rows.length && rows[i].type === 'add') adds.push(rows[i++]);
        if (!dels.length && !adds.length) { i++; continue; }
        for (let k = 0; k < Math.max(dels.length, adds.length); k++) html += '<div class="diff-row-pair">' + side(dels[k], 'left') + side(adds[k], 'right') + '</div>';
      }
    }
    html += '</div>';
  }
  return { html, remaining: Math.max(0, total - count) };
}
