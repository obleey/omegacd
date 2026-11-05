<script lang="ts">
	import { onMount } from 'svelte';
	import { api, ago, enc, errMsg, get, type APIToken, type HostStatus, type HostView, type RegistryCredential, type RepoCredential } from '#lib/api.ts';
	import Badge from '#lib/Badge.svelte';
	import Icon from '#lib/Icon.svelte';
	import { toastError } from '#lib/toast.svelte.ts';

	let repos = $state<RepoCredential[]>([]);
	let regs = $state<RegistryCredential[]>([]);
	let tokens = $state<APIToken[]>([]);
	let hosts = $state<HostView[]>([]);
	let error = $state('');

	// Forms: id 0 = create. Secret fields are write-only; empty keeps the stored value on edit.
	const blankRepo = () => ({ id: 0, urlPrefix: '', username: '', password: '', sshPrivateKey: '' });
	const blankReg = () => ({ id: 0, registry: '', username: '', password: '' });
	let repoForm = $state(blankRepo());
	let regForm = $state(blankReg());
	// Host form: edit = false creates; secret fields are write-only like credentials.
	const blankHost = () => ({ edit: false, name: '', url: '', workDir: '', sshPrivateKey: '', tlsCA: '', tlsCert: '', tlsKey: '', hasSSHKey: false, hasTLS: false });
	let hostForm = $state(blankHost());
	let testing = $state<Record<string, boolean>>({});
	let tokenName = $state('');
	let newToken = $state('');
	let copied = $state(false);

	async function load() {
		try {
			[repos, regs, tokens] = await Promise.all([
				get<RepoCredential[]>('/settings/repo-credentials'),
				get<RegistryCredential[]>('/settings/registry-credentials'),
				get<APIToken[]>('/settings/tokens')
			]).then((r) => r.map((x) => x ?? []) as [RepoCredential[], RegistryCredential[], APIToken[]]);
			hosts = (await get<HostView[]>('/settings/hosts')) ?? [];
		} catch (e) {
			error = errMsg(e);
		}
	}
	onMount(load);

	async function run(fn: () => Promise<unknown>) {
		error = '';
		try {
			await fn();
			await load();
			return true;
		} catch (e) {
			toastError(e);
			return false;
		}
	}

	const save = (kind: string, f: { id: number }) =>
		f.id ? api('PUT', `/settings/${kind}/${f.id}`, f) : api('POST', `/settings/${kind}`, f);

	async function saveRepo(e: SubmitEvent) {
		e.preventDefault();
		if (await run(() => save('repo-credentials', repoForm))) repoForm = blankRepo();
	}
	async function saveReg(e: SubmitEvent) {
		e.preventDefault();
		if (await run(() => save('registry-credentials', regForm))) regForm = blankReg();
	}
	async function saveHost(e: SubmitEvent) {
		e.preventDefault();
		const { edit, hasSSHKey, hasTLS, ...body } = hostForm;
		const ok = await run(() =>
			edit ? api('PUT', `/settings/hosts/${enc(body.name)}`, body) : api('POST', '/settings/hosts', body)
		);
		if (ok) hostForm = blankHost();
	}
	async function testHost(name: string) {
		error = '';
		testing[name] = true;
		try {
			const st = await api<HostStatus>('POST', `/settings/hosts/${enc(name)}/test`);
			hosts = hosts.map((h) => (h.name === name ? { ...h, status: st } : h));
		} catch (e) {
			toastError(e, `Testing Host ${name} failed`);
		} finally {
			testing[name] = false;
		}
	}
	function delHost(name: string) {
		if (confirm(`Delete Host “${name}”?`)) run(() => api('DELETE', `/settings/hosts/${enc(name)}`));
	}
	function del(kind: string, id: number, what: string) {
		if (confirm(`Delete ${what}?`)) run(() => api('DELETE', `/settings/${kind}/${id}`));
	}
	async function createToken(e: SubmitEvent) {
		e.preventDefault();
		copied = false;
		await run(async () => {
			const r = await api<{ token: APIToken; plaintext: string }>('POST', '/settings/tokens', { name: tokenName });
			newToken = r.plaintext;
			tokenName = '';
		});
	}
	async function copy() {
		await navigator.clipboard.writeText(newToken);
		copied = true;
	}
</script>

<svelte:head><title>Settings · OmegaCD</title></svelte:head>

<h1>Settings</h1>
<p class="muted small sub">Hosts, credentials and API tokens used by every Application.</p>
{#if error}<p class="error" role="alert">{error}</p>{/if}

<section class="card">
	<h2><span class="hicon"><Icon name="server" size={17} /></span>Hosts</h2>
	<p class="muted small">Docker engines Applications deploy to. Secrets are stored encrypted and never shown.</p>
	<div class="scroll-x"><table>
		<thead><tr><th>Name</th><th>URL</th><th>Connection</th><th>Applications</th><th><span class="sr-only">Actions</span></th></tr></thead>
		<tbody>
			{#each hosts as h (h.name)}
				{@const local = h.name === 'local'}
				<tr>
					<td>{h.name}</td>
					<td class="mono">{local ? 'local Docker socket' : h.url}</td>
					<td>
						<Badge status={h.status.connected ? 'Connected' : 'Disconnected'} label="Connection" />
						<div class="muted small">{h.status.connected ? `Docker ${h.status.serverVersion ?? ''}` : h.status.error}</div>
					</td>
					<td>{h.apps.join(', ') || '—'}</td>
					<td class="right">
						<button onclick={() => testHost(h.name)} disabled={testing[h.name]}>{testing[h.name] ? 'Testing…' : 'Test'}</button>
						{#if !local}
							<button onclick={() => (hostForm = { ...blankHost(), edit: true, name: h.name, url: h.url, workDir: h.workDir, hasSSHKey: h.hasSSHKey, hasTLS: h.hasTLS })}>Edit</button>
							<span title={h.apps.length ? 'Used by Applications; delete or move them first' : undefined}>
								<button class="danger" onclick={() => delHost(h.name)} disabled={h.apps.length > 0}>Delete</button>
							</span>
						{/if}
					</td>
				</tr>
			{/each}
		</tbody>
	</table></div>
	<form onsubmit={saveHost}>
		<h3>{hostForm.edit ? `Edit Host “${hostForm.name}”` : 'Add Host'}</h3>
		<div class="grid">
			<div class="field">
				<label for="h-name">Name</label>
				<input id="h-name" bind:value={hostForm.name} required disabled={hostForm.edit} pattern="[a-z0-9][a-z0-9_-]*" autocomplete="off" />
			</div>
			<div class="field">
				<label for="h-url">URL</label>
				<input id="h-url" bind:value={hostForm.url} required placeholder="ssh://deploy@server or tcp://server:2376" autocomplete="off" />
			</div>
			<div class="field">
				<label for="h-dir">Work dir</label>
				<input id="h-dir" bind:value={hostForm.workDir} placeholder="/var/lib/omegacd" autocomplete="off" />
				<span class="hint">Application files are copied here on the host. Keep it on disk (default) so they survive a reboot.</span>
			</div>
		</div>
		<p class="muted small">ssh:// hosts need their key in OmegaCD's known_hosts file.</p>
		<div class="field">
			<label for="h-ssh">SSH private key <span class="muted">({hostForm.hasSSHKey ? 'set' : 'not set'})</span></label>
			<textarea id="h-ssh" bind:value={hostForm.sshPrivateKey} rows="3" placeholder={hostForm.hasSSHKey ? 'unchanged' : '-----BEGIN OPENSSH PRIVATE KEY-----'}></textarea>
		</div>
		<p class="muted small">TLS client certificate for tcp:// hosts: {hostForm.hasTLS ? 'set' : 'not set'}</p>
		<div class="grid">
			<div class="field">
				<label for="h-ca">TLS CA</label>
				<textarea id="h-ca" bind:value={hostForm.tlsCA} rows="3" placeholder={hostForm.edit ? 'unchanged' : '-----BEGIN CERTIFICATE-----'}></textarea>
			</div>
			<div class="field">
				<label for="h-cert">TLS cert</label>
				<textarea id="h-cert" bind:value={hostForm.tlsCert} rows="3" placeholder={hostForm.hasTLS ? 'unchanged' : '-----BEGIN CERTIFICATE-----'}></textarea>
			</div>
			<div class="field">
				<label for="h-key">TLS key</label>
				<textarea id="h-key" bind:value={hostForm.tlsKey} rows="3" placeholder={hostForm.hasTLS ? 'unchanged' : '-----BEGIN PRIVATE KEY-----'}></textarea>
			</div>
		</div>
		<div class="row">
			<button class="primary" type="submit">{hostForm.edit ? 'Save' : 'Add'}</button>
			{#if hostForm.edit}<button type="button" onclick={() => (hostForm = blankHost())}>Cancel</button>{/if}
		</div>
	</form>
</section>

<section class="card">
	<h2><span class="hicon"><Icon name="folder" size={17} /></span>Repository credentials <span class="muted small">— for reading Git repositories</span></h2>
	<p class="muted small">Private Git repos (e.g. <code>https://github.com/you/</code> + a token with read access). Used for every Repository whose URL starts with the prefix. Secrets are stored encrypted and never shown.</p>
	{#if repos.length}
		<div class="scroll-x"><table>
			<thead><tr><th>URL prefix</th><th>Username</th><th>Password / token</th><th>SSH key</th><th>Used by</th><th><span class="sr-only">Actions</span></th></tr></thead>
			<tbody>
				{#each repos as c (c.id)}
					<tr>
						<td class="mono">{c.urlPrefix}</td>
						<td>{c.username || '—'}</td>
						<td>{c.hasPassword ? 'set' : 'not set'}</td>
						<td>{c.hasSSHKey ? 'set' : 'not set'}</td>
						<td>{c.usedBy?.join(', ') || '—'}</td>
						<td class="right">
							<button onclick={() => (repoForm = { ...blankRepo(), id: c.id, urlPrefix: c.urlPrefix, username: c.username ?? '' })}>Edit</button>
							<button class="danger" onclick={() => del('repo-credentials', c.id, c.urlPrefix)}>Delete</button>
						</td>
					</tr>
				{/each}
			</tbody>
		</table></div>
	{/if}
	<form onsubmit={saveRepo}>
		<h3>{repoForm.id ? 'Edit credential' : 'Add credential'}</h3>
		<div class="grid">
			<div class="field">
				<label for="rc-url">URL prefix</label>
				<input id="rc-url" bind:value={repoForm.urlPrefix} required placeholder="https://github.com/org/" autocomplete="off" />
			</div>
			<div class="field">
				<label for="rc-user">Username</label>
				<input id="rc-user" bind:value={repoForm.username} autocomplete="off" />
			</div>
			<div class="field">
				<label for="rc-pass">Password or token</label>
				<input id="rc-pass" type="password" bind:value={repoForm.password} autocomplete="new-password" placeholder={repoForm.id ? 'unchanged' : ''} />
			</div>
		</div>
		<div class="field">
			<label for="rc-ssh">SSH private key</label>
			<textarea id="rc-ssh" bind:value={repoForm.sshPrivateKey} rows="3" placeholder={repoForm.id ? 'unchanged' : '-----BEGIN OPENSSH PRIVATE KEY-----'}></textarea>
		</div>
		<div class="row">
			<button class="primary" type="submit">{repoForm.id ? 'Save' : 'Add'}</button>
			{#if repoForm.id}<button type="button" onclick={() => (repoForm = blankRepo())}>Cancel</button>{/if}
		</div>
	</form>
</section>

<section class="card">
	<h2><span class="hicon"><Icon name="container" size={17} /></span>Registry credentials <span class="muted small">— for pulling Docker images</span></h2>
	<p class="muted small">Private image registries (e.g. <code>ghcr.io</code>, <code>registry.example.com:5000</code>) — a host, not a URL. For Git repositories use Repository credentials above.</p>
	{#if regs.length}
		<div class="scroll-x"><table>
			<thead><tr><th>Registry</th><th>Username</th><th>Password</th><th><span class="sr-only">Actions</span></th></tr></thead>
			<tbody>
				{#each regs as c (c.id)}
					<tr>
						<td class="mono">{c.registry}</td>
						<td>{c.username}</td>
						<td>{c.hasPassword ? 'set' : 'not set'}</td>
						<td class="right">
							<button onclick={() => (regForm = { ...blankReg(), id: c.id, registry: c.registry, username: c.username })}>Edit</button>
							<button class="danger" onclick={() => del('registry-credentials', c.id, c.registry)}>Delete</button>
						</td>
					</tr>
				{/each}
			</tbody>
		</table></div>
	{/if}
	<form onsubmit={saveReg}>
		<h3>{regForm.id ? 'Edit credential' : 'Add credential'}</h3>
		<div class="grid">
			<div class="field">
				<label for="rg-host">Registry</label>
				<input id="rg-host" bind:value={regForm.registry} required placeholder="ghcr.io" pattern="[^/:\s]+(:[0-9]+)?" title="A registry host like ghcr.io or registry.example.com:5000 — not a URL. Git repositories go under Repository credentials." autocomplete="off" />
			</div>
			<div class="field">
				<label for="rg-user">Username</label>
				<input id="rg-user" bind:value={regForm.username} required autocomplete="off" />
			</div>
			<div class="field">
				<label for="rg-pass">Password</label>
				<input id="rg-pass" type="password" bind:value={regForm.password} autocomplete="new-password" required={!regForm.id} placeholder={regForm.id ? 'unchanged' : ''} />
			</div>
		</div>
		<div class="row">
			<button class="primary" type="submit">{regForm.id ? 'Save' : 'Add'}</button>
			{#if regForm.id}<button type="button" onclick={() => (regForm = blankReg())}>Cancel</button>{/if}
		</div>
	</form>
</section>

<section class="card">
	<h2><span class="hicon"><Icon name="key" size={17} /></span>API tokens</h2>
	<p class="muted small">Use as <code>Authorization: Bearer &lt;token&gt;</code>.</p>
	{#if newToken}
		<div class="newtoken" role="status">
			<p class="small">Copy this token now. It will not be shown again.</p>
			<div class="row">
				<code class="tok">{newToken}</code>
				<button onclick={copy}>{copied ? 'Copied' : 'Copy'}</button>
				<button class="ghost" onclick={() => (newToken = '')}>Dismiss</button>
			</div>
		</div>
	{/if}
	{#if tokens.length}
		<div class="scroll-x"><table>
			<thead><tr><th>Name</th><th>Created</th><th>Last used</th><th><span class="sr-only">Actions</span></th></tr></thead>
			<tbody>
				{#each tokens as t (t.id)}
					<tr>
						<td>{t.name}</td>
						<td>{ago(t.createdAt)}</td>
						<td>{t.lastUsed ? ago(t.lastUsed) : 'Never'}</td>
						<td class="right"><button class="danger" onclick={() => del('tokens', t.id, `token “${t.name}”`)}>Delete</button></td>
					</tr>
				{/each}
			</tbody>
		</table></div>
	{/if}
	<form class="row tokform" onsubmit={createToken}>
		<label class="sr-only" for="tk-name">Token name</label>
		<input id="tk-name" bind:value={tokenName} required placeholder="Token name, e.g. ci" autocomplete="off" />
		<button class="primary" type="submit">Create token</button>
	</form>
</section>

<style>
	.sub { margin: 0.15rem 0 1.2rem; }
	section { margin-bottom: 1.2rem; }
	h2 { display: flex; align-items: center; gap: 0.6rem; }
	.hicon { width: 32px; height: 32px; border-radius: 9px; display: grid; place-items: center; color: var(--accent); background: color-mix(in srgb, var(--accent) 13%, transparent); }
	form h3 { padding-top: 1rem; border-top: 1px dashed var(--border); }
	tbody tr:hover { background: color-mix(in srgb, var(--surface0) 35%, transparent); }
	section > p { margin: 0.2rem 0 0.8rem; }
	h3 { margin: 1.2rem 0 0.6rem; }
	.grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 0 1rem; }
	.right { text-align: right; white-space: nowrap; }
	.tokform { margin-top: 1rem; max-width: 520px; flex-wrap: nowrap; }
	.newtoken { border: 1px solid color-mix(in srgb, var(--green) 45%, transparent); background: color-mix(in srgb, var(--green) 10%, transparent); border-radius: var(--radius); padding: 0.6rem 0.8rem; margin-bottom: 1rem; }
	.newtoken p { margin: 0 0 0.4rem; }
	.tok { background: var(--crust); padding: 0.3rem 0.5rem; border-radius: 4px; word-break: break-all; flex: 1; }
</style>
