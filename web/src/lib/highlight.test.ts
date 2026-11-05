import { test } from 'node:test';
import assert from 'node:assert/strict';
import { langFor, lineToks, tokenize, type Lang } from './highlight.ts';

test('langFor', () => {
	const cases: [string, Lang | null][] = [
		['compose.yaml', 'yaml'],
		['apps/x/prometheus.yml', 'yaml'],
		['dashboard.json', 'json'],
		['.env', 'dotenv'],
		['grafana.env', 'dotenv'],
		['.env.local', 'dotenv'],
		['Caddyfile', 'caddyfile'],
		['sites/blog.caddy', 'caddyfile'],
		['Dockerfile', 'docker'],
		['nginx.conf', 'nginx'],
		['php/uploads.ini', 'ini'],
		['config.toml', 'toml'],
		['entrypoint.sh', 'shellscript'],
		['README', null],
		['logo.png', null]
	];
	for (const [name, want] of cases) assert.equal(langFor(name), want, name);
});

const samples: Record<string, [Lang, string]> = {
	compose: ['yaml', 'name: web\nservices:\n  web:\n    image: nginx:${TAG} # pinned\n    environment:\n      DB_PASSWORD: ••••\n    ports: ["8080:80"]\n\n    command: |\n      echo "a # b"\n'],
	caddy: ['caddyfile', '# comment\n{\n\tauto_https off\n}\n\nhttp://wp.localhost {\n\theader X-Served-By omegacd\n\treverse_proxy wordpress:80\n\t@api path /api/*\n\tredir https://{host}{uri} 301\n}\n'],
	env: ['dotenv', 'TAG=1.27\nDB_PASSWORD=••••\n# comment\nexport URL="http://x"\n'],
	ini: ['ini', '[PHP]\nupload_max_filesize = 64M ; big\n']
};

test('tokens rebuild every line exactly, and the code is colored', async () => {
	for (const [name, [lang, code]] of Object.entries(samples)) {
		const lines = await tokenize(code, lang);
		assert.deepEqual(
			lines.map((l) => l.map((t) => t.text).join('')),
			code.replace(/\n$/, '').split('\n'),
			name
		);
		const styles = new Set(lines.flat().map((t) => t.style));
		assert.ok(styles.size >= 3, `${name}: only ${styles.size} distinct colors`);
		assert.ok(lines.flat().every((t) => !t.style || (t.style.includes('--shiki-light') && t.style.includes('--shiki-dark'))), `${name}: missing a theme`);
	}
});

test('Caddyfile site addresses, directives and comments get distinct colors', async () => {
	const [comment, site, directive] = await tokenize('# note\nhttp://wp.localhost {\n\treverse_proxy wordpress:80\n}\n', 'caddyfile');
	const styles = [comment[0], site.find((t) => t.text.includes('wp.localhost')), directive.find((t) => t.text.includes('reverse_proxy'))].map((t) => t?.style);
	assert.ok(styles.every(Boolean), 'all three colored');
	assert.equal(new Set(styles).size, 3, 'three different colors');
});

test('lineToks falls back to the plain line when tokens are missing or stale', async () => {
	const toks = await tokenize('a: 1\nb: 2\n', 'yaml');
	assert.equal(lineToks(toks, 1, 'b: 2'), toks[1]);
	assert.deepEqual(lineToks(toks, 1, 'b: 3'), [{ text: 'b: 3' }]);
	assert.deepEqual(lineToks(null, 0, 'x'), [{ text: 'x' }]);
	assert.deepEqual(lineToks(toks, 9, 'y'), [{ text: 'y' }]);
});

test('.env keys are colored, also in a SOPS-decrypted file shown as KEY=••••', async () => {
	const [[key, eq, value]] = await tokenize('GF_SECURITY_ADMIN_PASSWORD=••••\n', 'dotenv');
	assert.equal(key.text, 'GF_SECURITY_ADMIN_PASSWORD');
	assert.match(key.style ?? '', /--shiki-dark:#89b4fa/i);
	assert.match(key.style ?? '', /--shiki-light:#1e66f5/i);
	assert.notEqual(key.style, value.style);
	assert.equal(eq.text, '=');
});

test('TOML keys are colored too', async () => {
	const [[key]] = await tokenize('port = 80\n', 'toml');
	assert.match(key.style ?? '', /--shiki-dark:#89b4fa/i);
});
