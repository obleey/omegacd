// Syntax colors via Shiki (VS Code's TextMate grammars, its whole catalogue of ~240 languages) with the
// Catppuccin themes. Everything is loaded on first use, one grammar at a time, so pages without code never
// download any of it. The JavaScript regex engine avoids shipping Oniguruma's WebAssembly.
import type { HighlighterCore, LanguageRegistration, ThemeRegistration } from 'shiki/core';

// style: CSS custom properties --shiki-light / --shiki-dark (and their -font-style), picked by app.css
export type Tok = { text: string; style?: string };

const byName: Record<string, string> = {
	caddyfile: 'caddyfile',
	dockerfile: 'docker',
	containerfile: 'docker',
	makefile: 'make',
	'.env': 'dotenv',
	'.htaccess': 'apache',
	'httpd.conf': 'apache',
	'apache2.conf': 'apache',
	jenkinsfile: 'groovy',
	'.gitignore': 'ini',
	'.dockerignore': 'ini'
};
const byExt: Record<string, string> = {
	env: 'dotenv',
	caddy: 'caddyfile',
	dockerfile: 'docker',
	cfg: 'ini',
	cnf: 'ini', // MySQL / MariaDB
	service: 'systemd',
	timer: 'systemd',
	socket: 'systemd',
	mount: 'systemd',
	target: 'systemd',
	tpl: 'go'
};

// langFor guesses a language from a file name (and, for *.conf, its content); tokenize checks the guess
// against Shiki's catalogue, whose ids and aliases cover most extensions as they are (py, php, sql, md, tf…).
export function langFor(name: string, content = ''): string | null {
	const base = name.split('/').pop()!.toLowerCase();
	const ext = base.includes('.') ? base.slice(base.lastIndexOf('.') + 1) : '';
	if (byName[base]) return byName[base];
	if (base.startsWith('.env.')) return 'dotenv';
	if (base.startsWith('dockerfile.')) return 'docker';
	if (ext === 'conf') return confLang(base, content);
	return byExt[ext] ?? (ext || null);
}

// *.conf is many formats: nginx and Apache by their telltale blocks, anything else as INI
function confLang(base: string, content: string): string {
	if (base.startsWith('nginx') || /^\s*(server|location|upstream|http|events|map)\b[^\n{;]*\{/m.test(content)) return 'nginx';
	if (base.startsWith('apache') || base.startsWith('httpd') || /^\s*<\/?(VirtualHost|Directory|IfModule|Location|Files)\b|^\s*(ServerName|DocumentRoot|LoadModule|RewriteEngine)\s/im.test(content))
		return 'apache';
	return 'ini';
}

// From caddyserver/vscode-caddyfile@9a294dc (MIT, see grammars/caddyfile.LICENSE); Shiki has none.
const caddyfile = () =>
	import('./grammars/caddyfile.tmLanguage.json', { with: { type: 'json' } }).then((m) => ({
		default: [{ ...(m.default as unknown as LanguageRegistration), name: 'caddyfile' }]
	}));

let core: Promise<HighlighterCore> | null = null;
const loaded = new Map<string, Promise<string | null>>();

function highlighter(): Promise<HighlighterCore> {
	core ??= (async () => {
		const [{ createHighlighterCore }, { createJavaScriptRegexEngine }] = await Promise.all([import('shiki/core'), import('shiki/engine/javascript')]);
		return createHighlighterCore({
			themes: [
				import('shiki/themes/catppuccin-latte.mjs').then((m) => withKeyColors(m.default, '#1e66f5')),
				import('shiki/themes/catppuccin-mocha.mjs').then((m) => withKeyColors(m.default, '#89b4fa'))
			],
			// forgiving: a grammar using a regex feature JavaScript lacks degrades instead of failing
			engine: createJavaScriptRegexEngine({ forgiving: true })
		});
	})();
	return core;
}

// Catppuccin has no color for the keys of the dotenv and TOML grammars, so those files came out as plain text;
// color them like INI keys (Catppuccin blue of the flavor).
function withKeyColors(theme: ThemeRegistration, blue: string): ThemeRegistration {
	return { ...theme, tokenColors: [...(theme.tokenColors ?? []), { scope: ['variable.key.dotenv', 'variable.other.key.toml'], settings: { foreground: blue } }] };
}

// load loads lang's grammar once and returns the id to highlight with, or null if Shiki has no such language
function load(h: HighlighterCore, lang: string): Promise<string | null> {
	if (!loaded.has(lang))
		loaded.set(
			lang,
			(async () => {
				if (lang === 'caddyfile') {
					await h.loadLanguage(...(await caddyfile()).default);
					return lang;
				}
				const { bundledLanguages, bundledLanguagesInfo } = await import('shiki/langs');
				const info = bundledLanguagesInfo.find((l) => l.id === lang || l.aliases?.includes(lang));
				if (!info) return null;
				await h.loadLanguage(...(await bundledLanguages[info.id as keyof typeof bundledLanguages]()).default);
				return info.id;
			})()
		);
	return loaded.get(lang)!;
}

// tokenize returns one token list per line of code, or null for a language Shiki doesn't know;
// joining a line's token texts gives the line back exactly.
export async function tokenize(code: string, lang: string): Promise<Tok[][] | null> {
	const h = await highlighter();
	const id = await load(h, lang);
	if (!id) return null;
	const { tokens } = h.codeToTokens(code.replace(/\n$/, ''), {
		lang: id,
		themes: { light: 'catppuccin-latte', dark: 'catppuccin-mocha' },
		defaultColor: false
	});
	return tokens.map((line) =>
		line.map((t) => ({
			text: t.content,
			style: Object.entries(t.htmlStyle ?? {})
				.map(([k, v]) => `${k}:${v}`)
				.join(';')
		}))
	);
}

// lineToks is line i of tokenize's result when it still matches text (tokens arrive async and may belong to
// older content), else the plain line: colors may lag, the text is always right.
export function lineToks(all: Tok[][] | null | undefined, i: number, text: string): Tok[] {
	const t = all?.[i];
	return t && t.map((x) => x.text).join('') === text ? t : [{ text }];
}
