export type DiffLine = { op: " " | "+" | "-"; text: string };

// lineDiff is a plain LCS line diff: the output is a, with lines only in b marked '+' and lines only in a '-'.
// shortcut: O(n·m) time and memory, fine for compose files (a few hundred lines); switch to Myers if that grows.
export function lineDiff(a: string, b: string): DiffLine[] {
  const x = a ? a.replace(/\n$/, "").split("\n") : [];
  const y = b ? b.replace(/\n$/, "").split("\n") : [];
  const n = x.length,
    m = y.length;
  // lcs[i][j] = length of the longest common subsequence of x[i:] and y[j:]
  const lcs = Array.from({ length: n + 1 }, () => new Uint32Array(m + 1));
  for (let i = n - 1; i >= 0; i--)
    for (let j = m - 1; j >= 0; j--)
      lcs[i][j] =
        x[i] === y[j]
          ? lcs[i + 1][j + 1] + 1
          : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
  const out: DiffLine[] = [];
  let i = 0,
    j = 0;
  while (i < n && j < m) {
    if (x[i] === y[j]) (out.push({ op: " ", text: x[i++] }), j++);
    else if (lcs[i + 1][j] >= lcs[i][j + 1])
      out.push({ op: "-", text: x[i++] });
    else out.push({ op: "+", text: y[j++] });
  }
  while (i < n) out.push({ op: "-", text: x[i++] });
  while (j < m) out.push({ op: "+", text: y[j++] });
  return out;
}
