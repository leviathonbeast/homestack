// A line diff for the review screen: config files are small, so a plain
// longest-common-subsequence table is fast enough and exact.
//
// Returns rows { t: ' ' | '+' | '-' | '…', text, a, b } where a and b are
// 1-based line numbers in the old and new text. Unchanged runs longer than
// 2 * context are folded into one '…' row.
export function lineDiff(before, after, context = 3) {
  const a = before ? before.replace(/\n$/, '').split('\n') : [];
  const b = after ? after.replace(/\n$/, '').split('\n') : [];
  const n = a.length, m = b.length;
  if (n * m > 4e6) {
    // Too big to compare line by line: show it as replaced.
    return [...a.map((text, k) => ({ t: '-', text, a: k + 1 })), ...b.map((text, k) => ({ t: '+', text, b: k + 1 }))];
  }
  const dp =Array.from({ length: n + 1 }, () => new Uint16Array(m + 1));
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      dp[i][j] = a[i] === b[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
    }
  }
  const rows = [];
  let i = 0, j = 0;
  while (i < n || j < m) {
    if (i < n && j < m && a[i] === b[j]) rows.push({ t: ' ', text: a[i], a: ++i, b: ++j });
    else if (j < m && (i === n || dp[i][j + 1] >= dp[i + 1][j])) rows.push({ t: '+', text: b[j], b: ++j });
    else rows.push({ t: '-', text: a[i], a: ++i });
  }
  // Fold long unchanged stretches.
  const out = [];
  for (let k = 0; k < rows.length; ) {
    if (rows[k].t !== ' ') { out.push(rows[k++]); continue; }
    let end = k;
    while (end < rows.length && rows[end].t === ' ') end++;
    const run = rows.slice(k, end);
    const lead = k === 0 ? 0 : context;
    const tail = end === rows.length ? 0 : context;
    if (run.length > lead + tail + 1) {
      out.push(...run.slice(0, lead), { t: '…', text: `${run.length - lead - tail} unchanged lines` }, ...run.slice(run.length - tail));
    } else out.push(...run);
    k = end;
  }
  return out;
}
