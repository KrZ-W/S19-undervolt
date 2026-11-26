// Shared chart helpers (index.html and setup.html).
// Charts are drawn 1:1 in CSS pixels (viewBox = the svg's real width x height, aspect kept), so text and dots are
// never squashed on a phone; every chart re-draws on resize.
const chartRedraw = new Map();
let chartResizeT = 0;
window.addEventListener('resize', () => { clearTimeout(chartResizeT); chartResizeT = setTimeout(() => chartRedraw.forEach(f => f()), 150); });

// chartFrame: sizes svg, returns scales + the axes / gridlines / tick numbers / axis titles as SVG markup.
// o = {x0, x1, y0, y1, xStep, yStep, xTitle, yTitle, xFmt, yFmt, H}
function chartFrame(svg, o) {
  const W = Math.max(260, Math.round(svg.clientWidth || (svg.parentNode && svg.parentNode.clientWidth) || 600)), H = o.H || 240;
  svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
  svg.removeAttribute('preserveAspectRatio');
  svg.style.height = H + 'px';
  const L = 50, R = 12, T = 10, B = 40, pw = W - L - R, ph = H - T - B;
  const X = x => +(L + (x - o.x0) / Math.max(1e-9, o.x1 - o.x0) * pw).toFixed(1);
  const Y = y => +(H - B - (y - o.y0) / Math.max(1e-9, o.y1 - o.y0) * ph).toFixed(1);
  let xs = o.xStep, ys = o.yStep;
  while ((o.x1 - o.x0) / xs * 42 > pw) xs *= 2; // at least ~42 px between x tick numbers
  while ((o.y1 - o.y0) / ys * 22 > ph) ys *= 2;
  const xf = o.xFmt || (v => String(+v.toFixed(2))), yf = o.yFmt || (v => String(+v.toFixed(2)));
  const txt = 'font-size="11" fill="currentColor" opacity=".65"';
  let g = '';
  for (let x = Math.ceil(o.x0 / xs - 1e-9) * xs; x <= o.x1 + 1e-9; x += xs)
    g += `<line x1="${X(x)}" x2="${X(x)}" y1="${T}" y2="${H - B}" stroke="currentColor" opacity=".08"/>` +
      `<text x="${X(x)}" y="${H - B + 14}" text-anchor="middle" ${txt}>${xf(x)}</text>`;
  for (let y = Math.ceil(o.y0 / ys - 1e-9) * ys; y <= o.y1 + 1e-9; y += ys)
    g += `<line x1="${L}" x2="${W - R}" y1="${Y(y)}" y2="${Y(y)}" stroke="currentColor" opacity=".08"/>` +
      `<text x="${L - 5}" y="${Y(y) + 4}" text-anchor="end" ${txt}>${yf(y)}</text>`;
  g += `<line x1="${L}" x2="${L}" y1="${T}" y2="${H - B}" stroke="currentColor" opacity=".35"/>` +
    `<line x1="${L}" x2="${W - R}" y1="${H - B}" y2="${H - B}" stroke="currentColor" opacity=".35"/>` +
    `<text x="${L + pw / 2}" y="${H - 4}" text-anchor="middle" font-size="12" fill="currentColor" opacity=".8">${o.xTitle}</text>` +
    `<text transform="translate(13 ${T + ph / 2}) rotate(-90)" text-anchor="middle" font-size="12" fill="currentColor" opacity=".8">${o.yTitle}</text>`;
  return { X, Y, grid: g, W, H };
}

// chartEmpty: a placeholder message, centred at the svg's real size.
function chartEmpty(svg, msg, H) {
  const W = Math.max(260, Math.round(svg.clientWidth || 600));
  svg.setAttribute('viewBox', `0 0 ${W} ${H}`); svg.removeAttribute('preserveAspectRatio'); svg.style.height = H + 'px';
  svg.innerHTML = `<text x="${W / 2}" y="${H / 2}" text-anchor="middle" font-size="13" fill="currentColor" opacity=".5">${msg}</text>`;
}

// Efficiency curve: J/TH against real (estimated) wall watts, stock vs trimmed.
// pts = {stock: [[watts, jth], ...], trimmed: [[watts, jth], ...]}
function effCurve(svg, legend, pts) {
  chartRedraw.set(svg, () => effCurve(svg, legend, pts));
  const all = [...pts.stock, ...pts.trimmed].filter(p => p[0] > 0 && p[1] > 0);
  if (all.length < 2) { chartEmpty(svg, 'efficiency curve appears after calibration', 220); legend.innerHTML = ''; return; }
  const x0 = Math.floor(Math.min(...all.map(p => p[0])) / 100) * 100, x1 = Math.max(x0 + 100, Math.ceil(Math.max(...all.map(p => p[0])) / 100) * 100);
  const y0 = Math.floor(Math.min(...all.map(p => p[1])) - 1), y1 = Math.ceil(Math.max(...all.map(p => p[1])) + 1);
  const f = chartFrame(svg, { x0, x1, y0, y1, xStep: x1 - x0 > 1000 ? 250 : 100, yStep: 1, H: 220,
    xTitle: 'Wall power (W, est.)', yTitle: 'Efficiency (J/TH)', xFmt: v => String(Math.round(v)), yFmt: v => String(Math.round(v)) });
  const line = (p, col, dash) => {
    const s = p.filter(q => q[0] > 0 && q[1] > 0).sort((a, b) => a[0] - b[0]);
    return `<polyline fill="none" stroke="${col}" stroke-width="2" ${dash ? 'stroke-dasharray="6 5"' : ''} points="${s.map(q => f.X(q[0]) + ',' + f.Y(q[1])).join(' ')}"/>` +
      s.map(q => `<circle cx="${f.X(q[0])}" cy="${f.Y(q[1])}" r="3" fill="${col}"><title>${Math.round(q[0])} W → ${q[1].toFixed(1)} J/TH</title></circle>`).join('');
  };
  svg.innerHTML = f.grid + line(pts.stock, 'var(--mute)', true) + line(pts.trimmed, 'var(--chart)', false);
  legend.innerHTML = '<span><i style="background:var(--mute)"></i>stock voltage</span><span><i style="background:var(--chart)"></i>trimmed</span>' +
    '<span>lower is better; wall watts estimated from the power model</span>';
}
