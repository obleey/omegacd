<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import {
		api,
		ago,
		enc,
		errMsg,
		get,
		short,
		type AppView,
		type Container,
		type DiffView,
		type LiveState,
		type SyncOperation
	} from '#lib/api.ts';
	import { live, putApp } from '#lib/live.svelte.ts';
	import Badge from '#lib/Badge.svelte';
	import Dialog from '#lib/Dialog.svelte';
	import AppForm from '#lib/AppForm.svelte';
	import Resources from '#lib/app/Resources.svelte';
	import Diff from '#lib/app/Diff.svelte';
	import History from '#lib/app/History.svelte';
	import Logs from '#lib/app/Logs.svelte';
	import Children from '#lib/app/Children.svelte';

	type Tab = 'resources' | 'diff' | 'history' | 'logs';

	let name = $derived(page.params.name ?? '');
	let app = $derived(live.apps[name] as AppView | undefined);
	let isApps = $derived(app?.kind === 'apps');
	// An App of Apps has no containers: its first tab lists Child Applications and there are no Logs.
	let tabs = $derived<[Tab, string][]>(
		isApps
			? [
					['resources', 'Child Applications'],
					['diff', 'Diff'],
					['history', 'History']
				]
			: [
					['resources', 'Resources'],
					['diff', 'Diff'],
					['history', 'History'],
					['logs', 'Logs']
				]
	);
	let children = $derived(
		Object.values(live.apps)
			.filter((a) => a.parent === name)
			.sort((a, b) => a.name.localeCompare(b.name))
	);
	// Child Application settings are owned by Git; it can only be deleted once its parent stops declaring it.
	let canDelete = $derived(!app?.parent || app.notInGit);
	let base = $derived(`/apps/${enc(name)}`);

	let tab = $state<Tab>('resources');
	let error = $state('');
	let notFound = $state(false);
	let busy = $state('');
	let editing = $state(false);
	let liveState = $state<LiveState | null>(null);
	let diff = $state<DiffView | null>(null);
	let ops = $state<SyncOperation[] | null>(null);
	let logContainer = $state('');

	let delOpen = $state(false);
	let cascade = $state(true);
	let rbOpen = $state(false);
	let rbOp = $state<SyncOperation | null>(null);

	let op = $derived(app?.lastOperation);
	let running = $derived(op?.phase === 'Running');
	let progress = $derived(op ? (live.progress[op.id] ?? []) : []);

	// Load the app on navigation (it may not be in the shared store yet).
	$effect(() => {
		const n = name;
		notFound = false;
		liveState = diff = ops = null;
		get<AppView>(`/apps/${enc(n)}`)
			.then(putApp)
			.catch((e) => (e.status === 404 ? (notFound = true) : (error = errMsg(e))));
	});

	// Refetch the active tab's data whenever the app changes (status, reconcile, operation).
	$effect(() => {
		const key = [app?.reconciledAt, app?.syncStatus, app?.health, op?.id, op?.phase];
		if (!app || !key) return;
		const n = name;
		const t = tab;
		const fail = (e: unknown) => (error = errMsg(e));
		if ((t === 'resources' && !isApps) || t === 'logs') get<LiveState>(`/apps/${enc(n)}/resources`).then((v) => (liveState = v), fail);
		if (t === 'diff') get<DiffView>(`/apps/${enc(n)}/diff`).then((v) => (diff = v), fail);
		if (t === 'history') get<SyncOperation[]>(`/apps/${enc(n)}/history`).then((v) => (ops = v ?? []), fail);
	});

	async function act(label: string, fn: () => Promise<unknown>) {
		error = '';
		busy = label;
		try {
			await fn();
		} catch (e) {
			error = errMsg(e);
		} finally {
			busy = '';
		}
	}

	const sync = () => act('sync', () => api('POST', `${base}/sync`));
	const refresh = () => act('refresh', async () => putApp(await api<AppView>('POST', `${base}/refresh`)));

	function del() {
		delOpen = false;
		act('delete', async () => {
			await api('DELETE', `${base}?cascade=${cascade}`);
			delete live.apps[name];
			goto('/');
		});
	}

	function rollback() {
		const o = rbOp;
		rbOpen = false;
		if (o) act('rollback', () => api('POST', `${base}/rollback`, { opID: o.id }));
	}

	function showLogs(c: Container) {
		logContainer = c.id;
		tab = 'logs';
	}

	function tabKey(e: KeyboardEvent) {
		const i = tabs.findIndex(([t]) => t === tab);
		const d = e.key === 'ArrowRight' ? 1 : e.key === 'ArrowLeft' ? -1 : 0;
		if (!d) return;
		tab = tabs[(i + d + tabs.length) % tabs.length][0];
		(document.getElementById(`tab-${tab}`) as HTMLElement | null)?.focus();
	}
</script>

<svelte:head><title>{name} · dockercd</title></svelte:head>

<p class="small"><a href="/">← Applications</a></p>

{#if notFound}
	<h1>{name}</h1>
	<p class="muted">This Application does not exist.</p>
{:else if !app}
	{#if error}<p class="error" role="alert">{error}</p>{:else}<p class="muted">Loading…</p>{/if}
{:else}
	<header class="card hdr">
		<div class="row">
			<h1>{app.name}</h1>
			{#if isApps}<span class="tag">App of Apps</span>{/if}
			<Badge status={app.syncStatus} label="Sync Status" />
			<Badge status={app.health} label="Health Status" />
			{#if app.notInGit}<span class="tag warn">No longer declared in Git</span>{/if}
			<span class="spacer"></span>
			<button class="primary" onclick={sync} disabled={!!busy || running}>{running ? 'Syncing…' : 'Sync'}</button>
			<button onclick={refresh} disabled={!!busy}>{busy === 'refresh' ? 'Refreshing…' : 'Refresh'}</button>
			<button onclick={() => (editing = !editing)} aria-expanded={editing} disabled={!!app.parent}>Edit settings</button>
			<a class="btn" href="/api/apps/{enc(app.name)}/export" download="{app.name}.yaml">Export YAML</a>
			<button class="danger" onclick={() => (delOpen = true)} disabled={!!busy || !canDelete}>Delete</button>
		</div>
		<dl class="meta">
			{#if !isApps}<div><dt>Host</dt><dd class="mono">{app.host || 'local'}</dd></div>{/if}
			<div><dt>Repository</dt><dd class="mono">{app.repoURL}</dd></div>
			<div><dt>Path</dt><dd class="mono">{app.path || '.'}</dd></div>
			<div><dt>Target Revision</dt><dd class="mono">{app.targetRevision} <span class="muted">({short(app.targetSHA)})</span></dd></div>
			<div><dt>Deployed Revision</dt><dd class="mono">{short(app.deployedSHA)}</dd></div>
			<div>
				<dt>Sync Policy</dt>
				<dd>
					{[app.autoSync && 'Auto-Sync', app.selfHeal && 'Self-Heal', app.prune && 'Prune'].filter(Boolean).join(' · ') ||
						'Manual'}
				</dd>
			</div>
			<div><dt>Reconciled</dt><dd>{ago(app.reconciledAt)}</dd></div>
		</dl>
		{#if app.syncMessage}<p class="small syncmsg">{app.syncMessage}</p>{/if}
	</header>

	{#if app.parent}
		<p class="banner" role="status">
			Managed by App of Apps <a href="/apps/{enc(app.parent)}">{app.parent}</a> — change its settings in Git.
			{#if app.notInGit}It is no longer declared in Git, so it can be deleted.{/if}
		</p>
	{/if}
	{#if app.rolledBack}
		<p class="banner" role="status">
			Auto-Sync was turned off by a Rollback. Re-enable it in Edit settings to follow the target Revision again.
		</p>
	{/if}
	{#if error}<p class="error" role="alert">{error}</p>{/if}

	{#if running && op}
		<section class="card progress" aria-live="polite" aria-label="Sync progress">
			<h2>Syncing <span class="mono muted small">{short(op.revision)}</span> <span class="muted small">({op.trigger})</span></h2>
			{#if progress.length === 0}
				<p class="muted small">Starting…</p>
			{:else}
				<ul>
					{#each progress as p (p.resource)}
						<li><span class="mono">{p.resource}</span> <span class="st">{p.status}</span> <span class="muted small">{p.text ?? ''}</span></li>
					{/each}
				</ul>
			{/if}
		</section>
	{:else if op?.phase === 'Failed'}
		<p class="error" role="status">Last Sync failed: {op.message}</p>
	{/if}

	{#if editing && !app.parent}
		<section class="card">
			<h2 class="mb">Edit settings</h2>
			<AppForm
				initial={app}
				editing
				submitLabel="Save"
				oncancel={() => (editing = false)}
				onsave={async (a) => {
					putApp(await api<AppView>('PUT', base, a));
					editing = false;
				}}
			/>
		</section>
	{:else}
		<div class="tabs" role="tablist" aria-label="Application views" tabindex="-1" onkeydown={tabKey}>
			{#each tabs as [t, label] (t)}
				<button
					id="tab-{t}"
					role="tab"
					class="tab"
					aria-selected={tab === t}
					aria-controls="panel"
					tabindex={tab === t ? 0 : -1}
					onclick={() => (tab = t)}>{label}</button
				>
			{/each}
		</div>
		<div id="panel" role="tabpanel" aria-labelledby="tab-{tab}" class="card">
			{#if tab === 'resources' && isApps}
				<Children apps={children} />
			{:else if tab === 'resources'}
				<Resources name={app.name} state={liveState} onlogs={showLogs} />
			{:else if tab === 'diff'}
				<Diff {diff} apps={isApps} />
			{:else if tab === 'history'}
				{#if app.parent}
					<p class="muted small">
						Rollback is not available for a Child Application: pin its targetRevision in Git, or roll back
						<a href="/apps/{app.parent}">{app.parent}</a>.
					</p>
				{/if}
				<History
					{ops}
					onrollback={app.parent
						? undefined
						: (o) => {
								rbOp = o;
								rbOpen = true;
							}}
				/>
			{:else if isApps}
				<p class="muted">An App of Apps has no containers.</p>
			{:else if liveState}
				<Logs app={app.name} containers={liveState.containers ?? []} bind:selected={logContainer} />
			{:else}
				<p class="muted">Loading…</p>
			{/if}
		</div>
	{/if}

	<Dialog bind:open={delOpen} title="Delete {app.name}?">
		<p>The Application is removed from dockercd.</p>
		<div class="check">
			<input id="cascade" type="checkbox" bind:checked={cascade} />
			<label for="cascade">Also remove containers and networks</label>
		</div>
		<p class="muted small">Volumes are always kept.</p>
		{#snippet actions()}
			<button onclick={() => (delOpen = false)}>Cancel</button>
			<button class="danger solid" onclick={del}>Delete</button>
		{/snippet}
	</Dialog>

	<Dialog bind:open={rbOpen} title="Rollback {app.name}?">
		<p>Sync to Revision <code>{short(rbOp?.revision)}</code> from {ago(rbOp?.startedAt)}.</p>
		<p class="muted small">This turns Auto-Sync off so the Rollback is not overwritten.</p>
		{#snippet actions()}
			<button onclick={() => (rbOpen = false)}>Cancel</button>
			<button class="primary" onclick={rollback}>Rollback</button>
		{/snippet}
	</Dialog>
{/if}

<style>
	.hdr { margin: 0.3rem 0 1rem; }
	.meta { display: grid; grid-template-columns: repeat(auto-fill, minmax(200px, 1fr)); gap: 0.6rem 1.2rem; margin: 1rem 0 0; }
	.meta dt { font-size: 0.75rem; color: var(--subtext0); text-transform: uppercase; letter-spacing: 0.03em; }
	.meta dd { margin: 0; word-break: break-all; }
	.syncmsg { margin: 0.8rem 0 0; color: var(--subtext1); }
	.banner, .error { margin: 0 0 1rem; }
	.progress { margin-bottom: 1rem; border-color: color-mix(in srgb, var(--yellow) 40%, transparent); }
	.progress ul { list-style: none; padding: 0; margin: 0.6rem 0 0; display: grid; gap: 0.2rem; max-height: 240px; overflow: auto; }
	.progress .st { color: var(--yellow); }
	.mb { margin-bottom: 1rem; }
	.tabs { display: flex; gap: 0.2rem; border-bottom: 1px solid var(--surface0); margin-bottom: -1px; }
	.tab { background: none; border: 1px solid transparent; border-bottom: 0; border-radius: var(--radius) var(--radius) 0 0; color: var(--subtext0); }
	.tab:hover:not(:disabled) { background: var(--surface0); }
	.tab[aria-selected='true'] { color: var(--text); background: var(--mantle); border-color: var(--surface0); box-shadow: inset 0 2px 0 var(--accent); }
	#panel { border-top-left-radius: 0; }
</style>
