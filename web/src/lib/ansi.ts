// Turns a log line into styled segments: ANSI SGR colors when present, otherwise a light
// highlighting of timestamp, log level and key= names. Output is plain data rendered as text
// (never HTML), so a hostile log line cannot inject markup.

export type Seg = { text: string; cls?: string; style?: string };
export type Line = { ts: string; segs: Seg[]; level: Level };
export type Level = '' | 'error' | 'warn' | 'info' | 'debug';

// 16 basic colors → Catppuccin tokens (normal, bright)
const BASIC = ['surface1', 'red', 'green', 'yellow', 'blue', 'pink', 'teal', 'subtext1'];
const BRIGHT = ['overlay1', 'maroon', 'green', 'peach', 'sapphire', 'mauve', 'sky', 'text'];

// Docker adds an RFC 3339 timestamp when logs are requested with timestamps.
const TS = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2}))\s?/;
const ANSI = /\x1b\[([0-9;]*)m/g;
const OTHER_ESC = /\x1b\[[0-9;?]*[A-Za-ln-z]|\x1b\][^\x07]*\x07|\x1b[()][A-Z0-9]/g;
const LEVEL =
	/\b(FATAL|PANIC|CRIT(?:ICAL)?|EMERG(?:ENCY)?|ALERT|ERROR|ERR|WARN(?:ING)?|INFO|NOTICE|DEBUG|DBG|TRACE)\b|"(?:level|lvl|severity)"\s*:\s*"(\w+)"/i;
const KEY = /(^|\s)([A-Za-z_][\w.-]*)=/g;

function levelOf(word: string): Level {
	const w = word.toLowerCase();
	if (/^(fatal|panic|crit|critical|emerg|emergency|alert|error|err)$/.test(w)) return 'error';
	if (/^warn(ing)?$/.test(w)) return 'warn';
	if (/^(info|notice)$/.test(w)) return 'info';
	if (/^(debug|dbg|trace)$/.test(w)) return 'debug';
	return '';
}

function color(n: number, bright: boolean) {
	return `color: var(--${(bright ? BRIGHT : BASIC)[n]})`;
}

function xterm256(n: number): string {
	if (n < 8) return `var(--${BASIC[n]})`;
	if (n < 16) return `var(--${BRIGHT[n - 8]})`;
	if (n >= 232) {
		const v = 8 + (n - 232) * 10;
		return `rgb(${v},${v},${v})`;
	}
	const i = n - 16;
	const c = (x: number) => (x === 0 ? 0 : 55 + x * 40);
	return `rgb(${c(Math.floor(i / 36))},${c(Math.floor(i / 6) % 6)},${c(i % 6)})`;
}

type Sgr = { fg?: string; bg?: string; bold?: boolean; dim?: boolean; italic?: boolean; underline?: boolean };

function applySgr(st: Sgr, params: string): Sgr {
	const p = params === '' ? [0] : params.split(';').map((x) => Number(x) || 0);
	const s = { ...st };
	for (let i = 0; i < p.length; i++) {
		const n = p[i];
		if (n === 0) Object.keys(s).forEach((k) => delete s[k as keyof Sgr]);
		else if (n === 1) s.bold = true;
		else if (n === 2) s.dim = true;
		else if (n === 3) s.italic = true;
		else if (n === 4) s.underline = true;
		else if (n === 22) s.bold = s.dim = false;
		else if (n === 23) s.italic = false;
		else if (n === 24) s.underline = false;
		else if (n >= 30 && n <= 37) s.fg = color(n - 30, false);
		else if (n >= 90 && n <= 97) s.fg = color(n - 90, true);
		else if (n === 39) delete s.fg;
		else if (n >= 40 && n <= 47) s.bg = `background: var(--${BASIC[n - 40]})`;
		else if (n >= 100 && n <= 107) s.bg = `background: var(--${BRIGHT[n - 100]})`;
		else if (n === 49) delete s.bg;
		else if ((n === 38 || n === 48) && p[i + 1] === 5) {
			const v = xterm256(p[i + 2] ?? 0);
			if (n === 38) s.fg = `color: ${v}`;
			else s.bg = `background: ${v}`;
			i += 2;
		} else if ((n === 38 || n === 48) && p[i + 1] === 2) {
			const v = `rgb(${p[i + 2] ?? 0},${p[i + 3] ?? 0},${p[i + 4] ?? 0})`;
			if (n === 38) s.fg = `color: ${v}`;
			else s.bg = `background: ${v}`;
			i += 4;
		}
	}
	return s;
}

function styleOf(s: Sgr): string | undefined {
	const parts = [s.fg, s.bg, s.bold && 'font-weight: 700', s.dim && 'opacity: 0.65', s.italic && 'font-style: italic', s.underline && 'text-decoration: underline'];
	const css = parts.filter(Boolean).join('; ');
	return css || undefined;
}

/** Strips every escape sequence: for copying, searching and level detection. */
export function stripAnsi(s: string): string {
	return s.replace(ANSI, '').replace(OTHER_ESC, '');
}

export function parseLine(raw: string): Line {
	let rest = raw;
	let ts = '';
	const m = TS.exec(rest);
	if (m) {
		ts = m[1];
		rest = rest.slice(m[0].length);
	}
	rest = rest.replace(OTHER_ESC, '');
	const plain = rest.replace(ANSI, '');
	const lm = LEVEL.exec(plain);
	const level = lm ? levelOf(lm[1] ?? lm[2] ?? '') : '';

	// ANSI-colored line: honor the program's own colors.
	if (rest.includes('\x1b[')) {
		const segs: Seg[] = [];
		let st: Sgr = {};
		let last = 0;
		for (const a of rest.matchAll(ANSI)) {
			if (a.index! > last) segs.push({ text: rest.slice(last, a.index), style: styleOf(st) });
			st = applySgr(st, a[1]);
			last = a.index! + a[0].length;
		}
		if (last < rest.length) segs.push({ text: rest.slice(last), style: styleOf(st) });
		return { ts, segs, level };
	}

	// Plain line: highlight the level word and key= names.
	const segs: Seg[] = [];
	let i = 0;
	const marks: { start: number; end: number; cls: string }[] = [];
	if (lm && level) marks.push({ start: lm.index, end: lm.index + lm[0].length, cls: `lv-${level}` });
	for (const k of plain.matchAll(KEY)) {
		const start = k.index! + k[1].length;
		if (!marks.some((m) => start < m.end && start + k[2].length > m.start)) marks.push({ start, end: start + k[2].length, cls: 'key' });
	}
	marks.sort((a, b) => a.start - b.start);
	for (const mk of marks) {
		if (mk.start > i) segs.push({ text: plain.slice(i, mk.start) });
		segs.push({ text: plain.slice(mk.start, mk.end), cls: mk.cls });
		i = mk.end;
	}
	if (i < plain.length) segs.push({ text: plain.slice(i) });
	return { ts, segs, level };
}
