// Syntax colors via Shiki (VS Code's TextMate grammars) with the Catppuccin themes. Everything is loaded on
// first use, one grammar at a time, so pages without code never download any of it. The JavaScript regex
// engine avoids shipping Oniguruma's WebAssembly.
import type { HighlighterCore, LanguageRegistration, ThemeRegistration } from 'shiki/core';

export type Lang = 'yaml' | 'json' | 'dotenv' | 'toml' | 'ini' | 'nginx' | 'docker' | 'shellscript' | 'caddyfile';
// style: CSS custom properties --shiki-light / --shiki-dark (and their -font-style), picked by app.css
export type Tok = { text: string; style?: string };

const grammars: Record<Lang, () => Promise<{ default: LanguageRegistration[] }>> = {
	yaml: () => import('shiki/langs/yaml.mjs'),
	json: () => import('shiki/langs/json.mjs'),
	dotenv: () => import('shiki/langs/dotenv.mjs'),
	toml: () => import('shiki/langs/toml.mjs'),
	ini: () => import('shiki/langs/ini.mjs'),
	nginx: () => import('shiki/langs/nginx.mjs'),
	docker: () => import('shiki/langs/docker.mjs'),
	shellscript: () => import('shiki/langs/shellscript.mjs'),
	// From caddyserver/vscode-caddyfile@9a294dc (MIT, see grammars/caddyfile.LICENSE); Shiki has none.
	caddyfile: () =>
		import('./grammars/caddyfile.tmLanguage.json', { with: { type: 'json' } }).then((m) => ({
			default: [{ ...(m.default as unknown as LanguageRegistration), name: 'caddyfile' }]
		}))
};

export function langFor(name: string): Lang | null {
	const base = name.split('/').pop()!.toLowerCase();
	const ext = base.includes('.') ? base.slice(base.lastIndexOf('.') + 1) : '';
	if (base === 'caddyfile' || ext === 'caddy' || ext === 'caddyfile') return 'caddyfile';
	if (base === 'dockerfile' || base.startsWith('dockerfile.') || ext === 'dockerfile') return 'docker';
	if (base === '.env' || base.startsWith('.env.') || ext === 'env') return 'dotenv';
	if (base.startsWith('nginx') && ext === 'conf') return 'nginx';
	const byExt: Record<string, Lang> = { yaml: 'yaml', yml: 'yaml', json: 'json', toml: 'toml', ini: 'ini', cfg: 'ini', conf: 'ini', sh: 'shellscript', bash: 'shellscript' };
	return byExt[ext] ?? null;
}

let core: Promise<HighlighterCore> | null = null;
const loaded = new Map<Lang, Promise<void>>();

function highlighter(): Promise<HighlighterCore> {
	core ??= (async () => {
		const [{ createHighlighterCore }, { createJavaScriptRegexEngine }] = await Promise.all([import('shiki/core'), import('shiki/engine/javascript')]);
		return createHighlighterCore({
			themes: [
				import('shiki/themes/catppuccin-latte.mjs').then((m) => withKeyColors(m.default, '#1e66f5')),
				import('shiki/themes/catppuccin-mocha.mjs').then((m) => withKeyColors(m.default, '#89b4fa'))
			],
			engine: createJavaScriptRegexEngine()
		});
	})();
	return core;
}

// Catppuccin has no color for the keys of the dotenv and TOML grammars, so those files came out as plain text;
// color them like INI keys (Catppuccin blue of the flavor).
function withKeyColors(theme: ThemeRegistration, blue: string): ThemeRegistration {
	return { ...theme, tokenColors: [...(theme.tokenColors ?? []), { scope: ['variable.key.dotenv', 'variable.other.key.toml'], settings: { foreground: blue } }] };
}

// tokenize returns one token list per line of code; joining a line's token texts gives the line back exactly.
export async function tokenize(code: string, lang: Lang): Promise<Tok[][]> {
	const h = await highlighter();
	if (!loaded.has(lang)) loaded.set(lang, grammars[lang]().then((g) => h.loadLanguage(...g.default)));
	await loaded.get(lang);
	const { tokens } = h.codeToTokens(code.replace(/\n$/, ''), {
		lang: lang === 'caddyfile' ? 'caddyfile' : lang,
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
