import { test } from 'node:test';
import assert from 'node:assert/strict';
import { langFor, lineToks, tokenize } from './highlight.ts';

test('langFor', () => {
	const cases: [string, string | null, string?][] = [
		['compose.yaml', 'yaml'],
		['apps/x/prometheus.yml', 'yml'],
		['dashboard.json', 'json'],
		['.env', 'dotenv'],
		['grafana.env', 'dotenv'],
		['.env.local', 'dotenv'],
		['Caddyfile', 'caddyfile'],
		['sites/blog.caddy', 'caddyfile'],
		['Dockerfile', 'docker'],
		['Dockerfile.prod', 'docker'],
		['nginx.conf', 'nginx'],
		['conf.d/default.conf', 'nginx', 'server {\n  listen 80;\n  location / { proxy_pass http://app; }\n}\n'],
		['sites/app.conf', 'apache', '<VirtualHost *:80>\n  ServerName example.com\n</VirtualHost>\n'],
		['other.conf', 'ini', 'key = value\n'],
		['.htaccess', 'apache'],
		['php/uploads.ini', 'ini'],
		['my.cnf', 'ini'],
		['config.toml', 'toml'],
		['README.md', 'md'],
		['index.php', 'php'],
		['init.sql', 'sql'],
		['app.py', 'py'],
		['main.tf', 'tf'],
		['app.properties', 'properties'],
		['omegacd.service', 'systemd'],
		['Makefile', 'make'],
		['README', null]
	];
	for (const [name, want, content] of cases) assert.equal(langFor(name, content), want, name);
});

const samples: [string, string][] = [
	['yaml', 'name: web\nservices:\n  web:\n    image: nginx:${TAG} # pinned\n    ports: ["8080:80"]\n\n    command: |\n      echo "a # b"\n'],
	['caddyfile', '# comment\n{\n\tauto_https off\n}\n\nhttp://wp.localhost {\n\treverse_proxy wordpress:80\n\tredir https://{host}{uri} 301\n}\n'],
	['dotenv', 'TAG=1.27\nDB_PASSWORD=••••\n# comment\nexport URL="http://x"\n'],
	['ini', '[PHP]\nupload_max_filesize = 64M ; big\n'],
	['nginx', 'server {\n  listen 80; # http\n  location / {\n    proxy_pass http://app:3000;\n  }\n}\n'],
	['apache', '<VirtualHost *:80>\n  ServerName example.com\n  # docs\n  DocumentRoot "/var/www"\n</VirtualHost>\n'],
	['md', '# Title\n\nSome *text* and `code`.\n\n- item\n'],
	['toml', '[server]\nport = 80 # http\nname = "x"\n'],
	['php', '<?php\n$x = "hi"; // say\necho $x;\n'],
	['py', 'import os\n\ndef f(x):\n    return x + 1  # one\n'],
	['sql', 'CREATE TABLE t (id INT PRIMARY KEY); -- t\nSELECT * FROM t;\n'],
	['js', 'const a = 1; // one\nexport default a;\n'],
	['lua', 'local x = 1 -- one\nprint(x)\n'],
	['xml', '<?xml version="1.0"?>\n<a b="c"><!-- d --></a>\n'],
	['html', '<!doctype html>\n<p class="x">hi</p>\n'],
	['tf', 'resource "aws_s3_bucket" "b" {\n  bucket = "x" # name\n}\n'],
	['properties', 'server.port=8080\n# comment\n'],
	['systemd', '[Service]\nExecStart=/usr/bin/omegacd\n'],
	['docker', '# tools\nFROM alpine:3.24\nRUN apk add curl\n']
];

test('every language rebuilds each line exactly and is colored', async () => {
	for (const [lang, code] of samples) {
		const lines = await tokenize(code, lang);
		assert.ok(lines, `${lang}: unknown`);
		assert.deepEqual(
			lines.map((l) => l.map((t) => t.text).join('')),
			code.replace(/\n$/, '').split('\n'),
			lang
		);
		const colors = new Set(lines.flat().map((t) => /--shiki-dark:(#\w+)/.exec(t.style ?? '')?.[1]?.toLowerCase()));
		colors.delete('#cdd6f4'); // plain text
		colors.delete(undefined);
		assert.ok(colors.size >= 2, `${lang}: only ${colors.size} highlight colors`);
		assert.ok(lines.flat().every((t) => !t.style || (t.style.includes('--shiki-light') && t.style.includes('--shiki-dark'))), `${lang}: missing a theme`);
	}
});

test('unknown languages stay plain', async () => {
	assert.equal(await tokenize('x', 'not-a-language'), null);
});

test('Caddyfile site addresses, directives and comments get distinct colors', async () => {
	const [comment, site, directive] = (await tokenize('# note\nhttp://wp.localhost {\n\treverse_proxy wordpress:80\n}\n', 'caddyfile'))!;
	const styles = [comment[0], site.find((t) => t.text.includes('wp.localhost')), directive.find((t) => t.text.includes('reverse_proxy'))].map((t) => t?.style);
	assert.ok(styles.every(Boolean), 'all three colored');
	assert.equal(new Set(styles).size, 3, 'three different colors');
});

test('lineToks falls back to the plain line when tokens are missing or stale', async () => {
	const toks = await tokenize('a: 1\nb: 2\n', 'yaml');
	assert.equal(lineToks(toks, 1, 'b: 2'), toks![1]);
	assert.deepEqual(lineToks(toks, 1, 'b: 3'), [{ text: 'b: 3' }]);
	assert.deepEqual(lineToks(null, 0, 'x'), [{ text: 'x' }]);
	assert.deepEqual(lineToks(toks, 9, 'y'), [{ text: 'y' }]);
});

test('.env keys are colored, also in a SOPS-decrypted file shown as KEY=••••', async () => {
	const [[key, eq, value]] = (await tokenize('GF_SECURITY_ADMIN_PASSWORD=••••\n', 'dotenv'))!;
	assert.equal(key.text, 'GF_SECURITY_ADMIN_PASSWORD');
	assert.match(key.style ?? '', /--shiki-dark:#89b4fa/i);
	assert.match(key.style ?? '', /--shiki-light:#1e66f5/i);
	assert.notEqual(key.style, value.style);
	assert.equal(eq.text, '=');
});

test('TOML keys are colored too', async () => {
	const [[key]] = (await tokenize('port = 80\n', 'toml'))!;
	assert.match(key.style ?? '', /--shiki-dark:#89b4fa/i);
});
