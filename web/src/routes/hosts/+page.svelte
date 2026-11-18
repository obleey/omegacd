<script lang="ts">
	// Hosts (Q152): every machine Applications deploy to, through its Agent; added here (M5-01), one card each (M5-02,
	// Q161), kept live (Q153).
	import { onMount } from 'svelte';
	import { ago, api, enc, errMsg, type HostView } from '#lib/api.ts';
	import AddHost from '#lib/AddHost.svelte';
	import Badge from '#lib/Badge.svelte';
	import Dialog from '#lib/Dialog.svelte';
	import { when } from '#lib/display.svelte.ts';
	import Empty from '#lib/Empty.svelte';
	import { shortKey, sopsYaml } from '#lib/hostkeys.ts';
	import { groupByParent, hostState, hostTone, lastSeen, parseAllowlist, renameBlocked } from '#lib/hosts.ts';
	import { hostBadges } from '#lib/badges.ts';
	import HostBadges from '#lib/HostBadges.svelte';
	import Icon from '#lib/Icon.svelte';
	import { live, loadHosts } from '#lib/live.svelte.ts';
	import { appTone } from '#lib/status.ts';
	import { toast, toastError } from '#lib/toast.svelte.ts';

	let error = $state('');
	let adding = $state(false);
	onMount(() => {
		loadHosts().catch((e) => (error = errMsg(e)));
	});
	let hosts = $derived(Object.values(live.hosts).sort((a, b) => a.name.localeCompare(b.name)));
	let allSops = $derived(sopsYaml(hosts.map((h) => ({ name: h.name, recipient: h.ageRecipient ?? '' }))));
	const path = (h: string, action = '') => `/settings/hosts/${enc(h)}${action && '/' + action}`;

	async function copyText(text: string, what: string) {
		try {
			await navigator.clipboard.writeText(text);
			toast('green', `${what} copied`);
		} catch (e) {
			toastError(e, `Copying ${what} failed`);
		}
	}
	async function rotate(h: HostView) {
		try {
			const r = await api<{ fingerprint: string }>('POST', path(h.name, 'rotate'));
			toast('green', `${h.name}: new key`, r.fingerprint);
		} catch (e) {
			toastError(e, `Rotating the key of ${h.name} failed`);
		}
	}

	// One confirmation dialog for what can't be taken back, or cuts an Agent off (Q164).
	let confirming = $state<{ title: string; body: string; label: string; danger?: boolean; run: () => Promise<unknown> } | null>(null);
	let confirmOpen = $state(false);
	let busy = $state(false);
	function ask(c: NonNullable<typeof confirming>) {
		confirming = c;
		confirmOpen = true;
	}
	async function confirmed() {
		if (!confirming) return;
		busy = true;
		try {
			await confirming.run();
			confirmOpen = false;
		} catch (e) {
			toastError(e, `${confirming.title} failed`);
		} finally {
			busy = false;
		}
	}
	const approve = (h: HostView) =>
		ask({
			title: `Approve ${h.name}`,
			body: `Approve only if the Agent on ${h.name} printed this same fingerprint: ${h.fingerprint}`,
			label: 'Approve',
			run: () => api('POST', path(h.name, 'approve'), { fingerprint: h.fingerprint })
		});
	const unlock = (h: HostView) =>
		ask({
			title: `Unlock ${h.name}`,
			body: `Two connections used its key at once: ${h.locked!.map((c) => `${c.address} since ${when(c.since)}`).join(' and ')}. Unlock only for a harmless cause, such as a cloned machine now gone; for a key that may be stolen, revoke the Host and add it again.`,
			label: 'Unlock',
			run: () => api('POST', path(h.name, 'unlock'))
		});
	const revoke = (h: HostView) =>
		ask({
			title: `Revoke ${h.name}`,
			body: `The Host is deleted and its Agent's key revoked: the Agent is cut off and must enroll again, with a new invite. Applications already on the Host keep running.`,
			label: 'Revoke',
			danger: true,
			run: () => api('DELETE', path(h.name))
		});

	// The allowlist editor (Q165) and renaming (Q162).
	let editing = $state<{ host: HostView; kind: 'allow' | 'rename'; text: string; error: string } | null>(null);
	let editOpen = $state(false);
	function edit(host: HostView, kind: 'allow' | 'rename') {
		editing = { host, kind, text: kind === 'allow' ? (host.allowIPs ?? []).join('\n') : host.name, error: '' };
		editOpen = true;
	}
	async function save(e: SubmitEvent) {
		e.preventDefault();
		if (!editing) return;
		busy = true;
		editing.error = '';
		try {
			if (editing.kind === 'allow') await api('PUT', path(editing.host.name, 'allow-ips'), { allowIPs: parseAllowlist(editing.text) });
			else await api('POST', path(editing.host.name, 'rename'), { name: editing.text.trim() });
			editOpen = false;
		} catch (err) {
			editing.error = errMsg(err);
		} finally {
			busy = false;
		}
	}
</script>

<svelte:head><title>Hosts · OmegaCD</title></svelte:head>

<div class="head">
	<div>
		<h1>Hosts</h1>
		<p class="muted small sub">The servers your Applications run on. Each runs an Agent that connects out to OmegaCD.
			Encrypt secrets for a Host with its Host Key, copied from its card into <code>.sops.yaml</code>: only that Host can decrypt them.</p>
	</div>
	<div class="row">
		{#if hosts.length}<button onclick={() => copyText(allSops, '.sops.yaml')}><Icon name="file-text" size={15} />Copy .sops.yaml for all Hosts</button>{/if}
		<button class="primary" onclick={() => (adding = true)}><Icon name="plus" size={15} />Add Host</button>
	</div>
</div>
{#if error}<p class="error" role="alert">{error}</p>{/if}

{#if live.hostsLoaded && !hosts.length}
	<Empty icon="server" title="No Hosts yet">
		<p>Add the first machine: OmegaCD gives you the command that enrolls its Agent.</p>
	</Empty>
{:else}
	<div class="grid">
		{#each hosts as h (h.name)}
			{@const state = hostState(h)}
			{@const seen = lastSeen(h)}
			<article class="card host {hostTone(h)}" aria-labelledby="host-{h.name}">
				<header>
					<div class="title">
						<h2 id="host-{h.name}"><Icon name="server" size={17} />{h.name}</h2>
						{#if h.agent?.hostname && h.agent.hostname !== h.name}<span class="muted small">{h.agent.hostname}</span>{/if}
						{#if seen && seen !== 'online'}<span class="muted small">last seen {ago(seen)}</span>{/if}
					</div>
					<div class="row badges">
						<Badge status={h.status.connected ? 'Connected' : 'Disconnected'} label="Connection" size="sm" />
						<Badge status={state} label="Enrollment" size="sm" />
						<HostBadges badges={hostBadges(h).filter((b) => b.status !== 'Host offline')} />
					</div>
				</header>

				{#if h.locked?.length}
					<div class="alert" role="alert">
						<p><Icon name="lock" size={15} />Locked: two connections used its key at once.</p>
						<ul>{#each h.locked as c (c.address + c.since)}<li><code>{c.address}</code> since {when(c.since)}</li>{/each}</ul>
						<div class="row">
							<button onclick={() => unlock(h)}>Unlock</button>
							<button class="danger" onclick={() => revoke(h)}>Revoke</button>
						</div>
					</div>
				{:else if h.agent?.refused}
					<p class="alert" role="alert"><Icon name="x-circle" size={15} />{h.agent.refused}</p>
				{/if}

				<dl>
					<dt>Agent</dt>
					<dd>{h.agent?.version ?? '—'}</dd>
					<dt>OS/arch</dt>
					<dd>{h.agent?.os ? `${h.agent.os}/${h.agent.arch}` : '—'}</dd>
					<dt>Applications</dt>
					<dd>
						{#if h.apps.length}
							<!-- grouped by the App of Apps that declares them (Q207): it heads its group, it doesn't run here -->
							{#each groupByParent(h.apps, (n) => live.apps[n]?.parent || undefined) as g (g.parent ?? '')}
								<span class="apps">
									{#if g.parent}
										{@const pa = live.apps[g.parent]}
										<a class="appchip {pa ? appTone(pa) : 'overlay'}" href="/apps/{enc(g.parent)}" title="App of Apps{pa ? ` · ${pa.syncStatus} · ${pa.health}` : ''}"><Icon name="apps" size={13} />{g.parent}</a>
									{/if}
									{#each g.apps as name (name)}
										{@const a = live.apps[name]}
										<a class="appchip {a ? appTone(a) : 'overlay'}" href="/apps/{enc(name)}" title={a ? `${a.syncStatus} · ${a.health}` : undefined}><Icon name="app" size={13} />{name}</a>
									{/each}
								</span>
							{/each}
						{:else}none{/if}
					</dd>
					<dt>Key</dt>
					<dd>{#if h.fingerprint}<code class="wrap">{h.fingerprint}</code>{:else}<span class="muted">when its Agent enrolls</span>{/if}</dd>
					<dt>Allowed from</dt>
					<dd>
						{h.allowIPs?.length ? h.allowIPs.join(', ') : 'anywhere'}
						{#if h.agent?.address}<div class="muted small">last from <code>{h.agent.address}</code></div>{/if}
					</dd>
					<dt>Host Key</dt>
					<dd>
						{#if h.ageRecipient}
							<code title={h.ageRecipient}>{shortKey(h.ageRecipient)}</code>
							<div class="row copies">
								<button class="sm" onclick={() => copyText(h.ageRecipient!, `Public key of ${h.name}`)} aria-label="Copy the public key of {h.name}">Copy key</button>
								<button class="sm" onclick={() => copyText(sopsYaml([{ name: h.name, recipient: h.ageRecipient! }]), `.sops.yaml rule for ${h.name}`)}
									aria-label="Copy the .sops.yaml rule for {h.name}">Copy .sops.yaml rule</button>
							</div>
						{:else}
							<span class="muted">when its Agent enrolls</span>
						{/if}
					</dd>
				</dl>

				<footer class="row">
					{#if state === 'To approve'}<button class="primary" onclick={() => approve(h)}><Icon name="check" size={15} />Approve…</button>{/if}
					<span title={h.status.connected ? undefined : 'Its Agent must be connected'}>
						<button onclick={() => rotate(h)} disabled={!h.status.connected || !h.approved}><Icon name="key" size={15} />Rotate key</button>
					</span>
					<button onclick={() => edit(h, 'allow')}>Allowlist…</button>
					<span title={renameBlocked(h)}><button onclick={() => edit(h, 'rename')} disabled={!!renameBlocked(h)}>Rename…</button></span>
					<span title={h.apps.length ? 'Used by Applications; delete or move them first' : undefined}>
						<button class="danger" onclick={() => revoke(h)} disabled={h.apps.length > 0}><Icon name="trash" size={15} />Revoke</button>
					</span>
				</footer>
			</article>
		{/each}
	</div>
{/if}

<AddHost bind:open={adding} />

<Dialog bind:open={confirmOpen} title={confirming?.title ?? ''}>
	<p class="wrap">{confirming?.body}</p>
	{#snippet actions()}
		<button onclick={() => (confirmOpen = false)}>Cancel</button>
		<button class={confirming?.danger ? 'danger' : 'primary'} onclick={confirmed} disabled={busy}>{confirming?.label}</button>
	{/snippet}
</Dialog>

<Dialog bind:open={editOpen} title={editing?.kind === 'allow' ? `Allowlist of ${editing.host.name}` : `Rename ${editing?.host.name ?? ''}`}>
	{#if editing}
		<form id="hostedit" onsubmit={save}>
			{#if editing.kind === 'allow'}
				<div class="field">
					<label for="he-allow">Addresses its Agent may connect from <span class="hint">one address or CIDR per line; none: anywhere</span></label>
					<textarea id="he-allow" rows="4" bind:value={editing.text} placeholder="203.0.113.7&#10;10.0.0.0/8"></textarea>
				</div>
				{#if editing.host.agent?.address}<p class="muted small">Its Agent last connected from <code>{editing.host.agent.address}</code>: leave it in, or the Agent is cut off.</p>{/if}
			{:else}
				<div class="field">
					<label for="he-name">New name</label>
					<input id="he-name" bind:value={editing.text} required pattern="[a-z0-9][a-z0-9_-]{'{0,62}'}" autocomplete="off" />
				</div>
				<p class="muted small">Its Agent reconnects under the new name.</p>
			{/if}
			{#if editing.error}<p class="error" role="alert">{editing.error}</p>{/if}
		</form>
	{/if}
	{#snippet actions()}
		<button onclick={() => (editOpen = false)}>Cancel</button>
		<button type="submit" form="hostedit" class="primary" disabled={busy}>Save</button>
	{/snippet}
</Dialog>

<style>
	.head { display: flex; align-items: flex-start; justify-content: space-between; gap: 1rem; flex-wrap: wrap; margin-bottom: 1.2rem; }
	.sub { margin: 0.15rem 0 0; max-width: 70ch; }
	.grid { display: grid; gap: 1rem; grid-template-columns: repeat(auto-fill, minmax(min(100%, 24rem), 1fr)); }
	/* the status bar on the left, as an Application's tile has */
	.host { display: grid; gap: 0.8rem; align-content: start; min-width: 0; position: relative; overflow: hidden; padding-left: 1.4rem;
		transition: transform 0.2s var(--ease), box-shadow 0.2s, border-color 0.2s; }
	.host::before { content: ''; position: absolute; left: 0; top: 0; bottom: 0; width: 4px; background: var(--c); transition: background-color 0.4s; }
	.host:hover { transform: translateY(-2px); box-shadow: var(--shadow-md); border-color: color-mix(in srgb, var(--c) 40%, var(--border)); }
	.host:focus-within { border-color: color-mix(in srgb, var(--accent) 60%, var(--border)); }
	.green { --c: var(--green); }
	.blue { --c: var(--blue); }
	.yellow { --c: var(--yellow); }
	.red { --c: var(--red); }
	.overlay { --c: var(--overlay1); }
	header { display: flex; justify-content: space-between; align-items: flex-start; gap: 0.6rem; flex-wrap: wrap; }
	.title { display: grid; gap: 0.1rem; }
	h2 { margin: 0; font-size: 1.05rem; display: flex; align-items: center; gap: 0.4rem; }
	.badges { gap: 0.35rem; }
	dl { display: grid; grid-template-columns: max-content minmax(0, 1fr); gap: 0.35rem 0.9rem; margin: 0; font-size: 0.88rem; }
	dt { color: var(--subtext0); }
	dd { margin: 0; min-width: 0; }
	.wrap { overflow-wrap: anywhere; }
	.copies { margin-top: 0.3rem; gap: 0.35rem; }
	/* an Application on this Host, its dot in its tile's color */
	.apps { display: flex; flex-wrap: wrap; align-items: center; gap: 0.3rem; }
	.apps + .apps { margin-top: 0.35rem; }
	.appchip { display: inline-flex; align-items: center; gap: 0.3rem; padding: 0.05rem 0.5rem 0.05rem 0.45rem; border: 1px solid var(--border);
		border-radius: 999px; color: var(--text); font-size: 0.8rem; text-decoration: none; transition: border-color 0.2s; }
	.appchip::before { content: ''; width: 0.45rem; height: 0.45rem; border-radius: 50%; background: var(--c); order: -1; }
	.appchip:hover { border-color: color-mix(in srgb, var(--c) 50%, var(--border)); }
	button.sm { padding: 0.15rem 0.5rem; font-size: 0.78rem; }
	footer { border-top: 1px solid var(--border); padding-top: 0.8rem; gap: 0.4rem; }
	.alert { margin: 0; color: var(--red); background: color-mix(in srgb, var(--red) 10%, transparent); border: 1px solid color-mix(in srgb, var(--red) 35%, transparent);
		border-radius: var(--radius); padding: 0.55rem 0.85rem; font-size: 0.88rem; overflow-wrap: anywhere; }
	.alert p, p.alert { display: flex; gap: 0.4rem; align-items: center; margin: 0; }
	.alert ul { margin: 0.4rem 0 0.6rem; padding-left: 1.2rem; color: var(--text); }
	textarea { width: 100%; font-family: var(--mono, monospace); }
</style>
