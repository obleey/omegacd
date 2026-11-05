export type Tok = { text: string; cls?: string }; // cls: a tk-* class from app.css

// highlight colors one line of a rendered compose file.
// shortcut: line by line, so YAML block scalars (|, >) and multi-line quotes are colored per line only;
// good enough for compose files, swap for a real tokenizer if that ever matters.
export function highlight(line: string): Tok[] {
	return line === '' ? [{ text: line }] : yamlLine(line);
}

const yamlKey = /^(\s*)((?:- +)*)("[^"]*"|'[^']*'|[^\s#'"-][^:#]*?|-[^\s:#][^:#]*?)(:)(?=\s|$)/;

function yamlLine(line: string): Tok[] {
	const out: Tok[] = [];
	let m = /^(\s*)(#.*)$/.exec(line);
	if (m) {
		push(out, m[1]);
		out.push({ text: m[2], cls: 'tk-comment' });
		return out;
	}
	let rest = line;
	if ((m = yamlKey.exec(line))) {
		push(out, m[1]);
		if (m[2]) out.push({ text: m[2], cls: 'tk-punct' });
		out.push({ text: m[3], cls: 'tk-key' }, { text: m[4], cls: 'tk-punct' });
		rest = line.slice(m[0].length);
	} else if ((m = /^(\s*)((?:- +)+|-$)/.exec(line))) {
		push(out, m[1]);
		out.push({ text: m[2], cls: 'tk-punct' });
		rest = line.slice(m[0].length);
	}
	const c = commentAt(rest);
	const [, lead, value, trail] = /^(\s*)(.*?)(\s*)$/.exec(c < 0 ? rest : rest.slice(0, c))!;
	push(out, lead);
	scalar(out, value);
	push(out, trail);
	if (c >= 0) out.push({ text: rest.slice(c), cls: 'tk-comment' });
	return out;
}

// a value: quoted string, number, bool/null, or plain text; ${VARS} and masked •••• stand out inside it
function scalar(out: Tok[], v: string) {
	if (!v) return;
	if (/^(true|false|null|yes|no|on|off|~)$/i.test(v)) return void out.push({ text: v, cls: 'tk-bool' });
	if (/^[-+]?(\d[\d_]*(\.\d+)?([eE][-+]?\d+)?|0x[\da-fA-F]+)$/.test(v)) return void out.push({ text: v, cls: 'tk-num' });
	const cls = /^["']/.test(v) ? 'tk-str' : /^[[{|>&*!]/.test(v) ? undefined : 'tk-val';
	for (const part of v.split(/(\$\{[^}]*\}|\$[A-Za-z_]\w*|•+)/)) {
		if (part) out.push({ text: part, cls: part.startsWith('•') ? 'tk-masked' : part.startsWith('$') ? 'tk-var' : cls });
	}
}

// index of a trailing YAML comment (" #" outside quotes), or -1
function commentAt(s: string): number {
	let q = '';
	for (let i = 0; i < s.length; i++) {
		const ch = s[i];
		if (q) q = ch === q ? '' : q;
		else if (ch === '"' || ch === "'") q = ch;
		else if (ch === '#' && (i === 0 || /\s/.test(s[i - 1]))) return i;
	}
	return -1;
}

function push(out: Tok[], text: string) {
	if (text) out.push({ text });
}
