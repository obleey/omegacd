<script lang="ts">
	import { onMount } from 'svelte';
	import { api, ago, errMsg, get, type APIToken, type RegistryCredential, type RepoCredential } from '#lib/api.ts';

	let repos = $state<RepoCredential[]>([]);
	let regs = $state<RegistryCredential[]>([]);
	let tokens = $state<APIToken[]>([]);
	let error = $state('');

	// Forms: id 0 = create. Secret fields are write-only; empty keeps the stored value on edit.
	const blankRepo = () => ({ id: 0, urlPrefix: '', username: '', password: '', sshPrivateKey: '' });
	const blankReg = () => ({ id: 0, registry: '', username: '', password: '' });
	let repoForm = $state(blankRepo());
	let regForm = $state(blankReg());
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
			error = errMsg(e);
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

<svelte:head><title>Settings · dockercd</title></svelte:head>

<h1>Settings</h1>
{#if error}<p class="error" role="alert">{error}</p>{/if}

<section class="card">
	<h2>Repository credentials</h2>
	<p class="muted small">Used for every Repository whose URL starts with the prefix. Secrets are stored encrypted and never shown.</p>
	{#if repos.length}
		<table>
			<thead><tr><th>URL prefix</th><th>Username</th><th>Password / token</th><th>SSH key</th><th><span class="sr-only">Actions</span></th></tr></thead>
			<tbody>
				{#each repos as c (c.id)}
					<tr>
						<td class="mono">{c.urlPrefix}</td>
						<td>{c.username || '—'}</td>
						<td>{c.hasPassword ? 'set' : 'not set'}</td>
						<td>{c.hasSSHKey ? 'set' : 'not set'}</td>
						<td class="right">
							<button onclick={() => (repoForm = { ...blankRepo(), id: c.id, urlPrefix: c.urlPrefix, username: c.username ?? '' })}>Edit</button>
							<button class="danger" onclick={() => del('repo-credentials', c.id, c.urlPrefix)}>Delete</button>
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
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
	<h2>Registry credentials</h2>
	<p class="muted small">Used to pull images during Sync.</p>
	{#if regs.length}
		<table>
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
		</table>
	{/if}
	<form onsubmit={saveReg}>
		<h3>{regForm.id ? 'Edit credential' : 'Add credential'}</h3>
		<div class="grid">
			<div class="field">
				<label for="rg-host">Registry</label>
				<input id="rg-host" bind:value={regForm.registry} required placeholder="ghcr.io" autocomplete="off" />
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
	<h2>API tokens</h2>
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
		<table>
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
		</table>
	{/if}
	<form class="row tokform" onsubmit={createToken}>
		<label class="sr-only" for="tk-name">Token name</label>
		<input id="tk-name" bind:value={tokenName} required placeholder="Token name, e.g. ci" autocomplete="off" />
		<button class="primary" type="submit">Create token</button>
	</form>
</section>

<style>
	h1 { margin-bottom: 1rem; }
	section { margin-bottom: 1.2rem; }
	section > p { margin: 0.2rem 0 0.8rem; }
	h3 { margin: 1.2rem 0 0.6rem; }
	.grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 0 1rem; }
	.right { text-align: right; white-space: nowrap; }
	.tokform { margin-top: 1rem; max-width: 520px; flex-wrap: nowrap; }
	.newtoken { border: 1px solid color-mix(in srgb, var(--green) 45%, transparent); background: color-mix(in srgb, var(--green) 10%, transparent); border-radius: var(--radius); padding: 0.6rem 0.8rem; margin-bottom: 1rem; }
	.newtoken p { margin: 0 0 0.4rem; }
	.tok { background: var(--crust); padding: 0.3rem 0.5rem; border-radius: 4px; word-break: break-all; flex: 1; }
</style>
