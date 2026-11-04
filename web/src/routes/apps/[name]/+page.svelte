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
	import { fade } from 'svelte/transition';
	import Badge from '#lib/Badge.svelte';
	import Dialog from '#lib/Dialog.svelte';
	import Icon, { type IconName } from '#lib/Icon.svelte';
	import Empty from '#lib/Empty.svelte';
	import SyncProgress from '#lib/app/SyncProgress.svelte';
	import SyncPolicy from '#lib/app/SyncPolicy.svelte';
	import { ms, shownSync } from '#lib/status.ts';
	import { toastError } from '#lib/toast.svelte.ts';
	import AppForm from '#lib/AppForm.svelte';
	import { refreshApp } from '#lib/actions.ts';
	import Resources from '#lib/app/Resources.svelte';
	import Diff from '#lib/app/Diff.svelte';
	import History from '#lib/app/History.svelte';
	import Logs from '#lib/app/Logs.svelte';
	import Children from '#lib/app/Children.svelte';

	type Tab = 'resources' | 'diff' | 'history' | 'logs';
	const tabIcon: Record<Tab, IconName> = { resources: 'tree', diff: 'diff', history: 'clock', logs: 'terminal' };

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
	// The form opens below the header and status panels, often outside the viewport: bring it into view.
	let editEl = $state<HTMLElement>();
	$effect(() => {
		if (!editing || !editEl) return;
		const reduce = matchMedia('(prefers-reduced-motion: reduce)').matches;
		editEl.scrollIntoView({ behavior: reduce ? 'auto' : 'smooth', block: 'start' });
		editEl.querySelector<HTMLElement>('input:not([disabled]), select:not([disabled]), textarea')?.focus({ preventScroll: true });
	});
	let liveState = $state<LiveState | null>(null);
	let diff = $state<DiffView | null>(null);
	let ops = $state<SyncOperation[] | null>(null);
	let logContainer = $state('');

	let delOpen = $state(false);
	let cascade = $state(true);
	let rbOpen = $state(false);
	let syncOpen = $state(false);
	let syncPrune = $state(false);
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
			toastError(e, `${label[0].toUpperCase()}${label.slice(1)} of ${name} failed`);
		} finally {
			busy = '';
		}
	}

	function openSync() {
		syncPrune = app?.prune ?? false;
		syncOpen = true;
	}
	function sync() {
		syncOpen = false;
		act('sync', () => api('POST', `${base}/sync`, { prune: syncPrune }));
	}
	const refresh = () => act('refresh', () => refreshApp(name));

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

<svelte:head><title>{name} · OmegaCD</title></svelte:head>

<nav class="crumbs small" aria-label="Breadcrumb">
	<a href="/">Applications</a>
	{#if app?.parent}<Icon name="chevron" size={12} /><a href="/apps/{enc(app.parent)}">{app.parent}</a>{/if}
	<Icon name="chevron" size={12} /><span aria-current="page">{name}</span>
</nav>

{#if notFound}
	<div class="card">
		<Empty icon="ghost" title="Application not found">
			<p>There is no Application named “{name}”.</p>
			<a class="btn" href="/">Back to Applications</a>
		</Empty>
	</div>
{:else if !app}
	{#if error}<p class="error" role="alert">{error}</p>{:else}
		<div class="card hdr" aria-busy="true" aria-label="Loading Application">
			<span class="skel" style="width: 240px; height: 30px"></span>
			<div class="panels">{#each [0, 1, 2] as i (i)}<span class="skel" style="height: 92px"></span>{/each}</div>
		</div>
	{/if}
{:else}
	<header class="card hdr">
		<div class="title">
			<span class="appicon"><Icon name={isApps ? 'apps' : 'app'} size={22} /></span>
			<h1>{app.name}</h1>
			{#if isApps}<span class="tag">App of Apps</span>{/if}
			{#if app.parent}<span class="tag">Child Application</span>{/if}
			{#if app.notInGit}<span class="tag warn">No longer declared in Git</span>{/if}
			<span class="spacer"></span>
			<div class="actions">
				<button class="primary" onclick={openSync} disabled={!!busy || running}><Icon name="sync" size={15} spin={running} />{running ? 'Syncing…' : 'Sync'}</button>
				<button onclick={refresh} disabled={!!busy}><Icon name="refresh" size={15} spin={busy === 'refresh'} />Refresh</button>
				<button onclick={() => (editing = !editing)} aria-expanded={editing} disabled={!!app.parent} title={app.parent ? 'Settings of a Child Application are owned by Git' : undefined}><Icon name="edit" size={15} />Edit</button>
				<a class="btn" href="/api/apps/{enc(app.name)}/export" download="{app.name}.yaml"><Icon name="download" size={15} />Export YAML</a>
				<button class="danger" onclick={() => (delOpen = true)} disabled={!!busy || !canDelete}><Icon name="trash" size={15} />Delete</button>
			</div>
		</div>

		<div class="panels">
			<section class="panel" aria-label="Health Status">
				<span class="plabel">Health Status</span>
				<Badge status={app.health} size="lg" label="Health Status" />
				<span class="muted small">Reconciled {ago(app.reconciledAt)}</span>
			</section>
			<section class="panel" aria-label="Sync Status">
				<span class="plabel">Sync Status</span>
				<Badge status={shownSync(app)} size="lg" label="Sync Status" />
				<span class="muted small">
					to <span class="mono">{app.targetRevision}</span> ({short(app.targetSHA)})
					{#if app.autoSync}· Auto-Sync{/if}
				</span>
			</section>
			<section class="panel" aria-label="Last Sync">
				<span class="plabel">Last Sync</span>
				{#if op}
					<Badge status={op.phase} size="lg" label="Last Sync Operation" text={op.phase === 'Running' ? 'Syncing' : op.phase === 'Succeeded' ? 'Sync OK' : 'Sync failed'} />
					<span class="muted small">{op.trigger} · {ago(op.finishedAt || op.startedAt)} · <span class="mono">{short(op.revision)}</span></span>
				{:else}
					<Badge status="Unknown" size="lg" text="Never synced" />
					<span class="muted small">Sync to deploy the Desired State</span>
				{/if}
			</section>
		</div>

		<dl class="meta">
			{#if !isApps}<div><dt><Icon name="server" size={12} />Host</dt><dd class="mono">{app.host || 'local'}</dd></div>{/if}
			<div class="wide"><dt><Icon name="folder" size={12} />Repository</dt><dd class="mono">{app.repoURL}</dd></div>
			<div><dt><Icon name="folder" size={12} />Path</dt><dd class="mono">{app.path || '.'}</dd></div>
			<div><dt><Icon name="branch" size={12} />Target Revision</dt><dd class="mono">{app.targetRevision} <span class="muted">({short(app.targetSHA)})</span></dd></div>
			<div><dt><Icon name="commit" size={12} />Deployed Revision</dt><dd class="mono">{short(app.deployedSHA)}</dd></div>
		</dl>
		{#if app.syncMessage}<p class="small syncmsg"><Icon name="info" size={14} />{app.syncMessage}</p>{/if}
		<SyncPolicy {app} />
	</header>

	{#if app.prunePending?.length}
		<p class="note" role="status">
			<Icon name="info" size={16} />
			<span>Not declared, stopped: <strong class="mono">{app.prunePending.join(', ')}</strong> — Sync with Prune to delete them.</span>
		</p>
	{/if}
	{#if app.parent}
		<p class="banner" role="status">
			<Icon name="info" size={16} />
			<span>
				Managed by App of Apps <a href="/apps/{enc(app.parent)}">{app.parent}</a> — change its settings in Git.
				{#if app.notInGit}It is no longer declared in Git, so it can be deleted.{/if}
			</span>
		</p>
	{/if}
	{#if app.rolledBack}
		<p class="banner" role="status">
			<Icon name="undo" size={16} />
			<span>Auto-Sync was turned off by a Rollback. Turn it back on in Sync Policy to follow the target Revision again.</span>
		</p>
	{/if}
	{#if error}<p class="error" role="alert"><Icon name="alert" size={16} />{error}</p>{/if}

	{#if running && op}
		<SyncProgress {op} {progress} />
	{:else if op?.phase === 'Failed'}
		<p class="error" role="status" in:fade={{ duration: ms(250) }}><Icon name="x-circle" size={16} /><span><strong>Last Sync failed:</strong> {op.message}</span></p>
	{/if}

	{#if editing && !app.parent}
		<section class="card" bind:this={editEl}>
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
					onclick={() => (tab = t)}><Icon name={tabIcon[t]} size={15} />{label}</button
				>
			{/each}
		</div>
		<div id="panel" role="tabpanel" aria-labelledby="tab-{tab}" class="card">
			{#if tab === 'resources' && isApps}
				<Children {app} apps={children} />
			{:else if tab === 'resources'}
				<Resources name={app.name} state={liveState} syncStatus={shownSync(app)} prunePending={app.prunePending} onlogs={showLogs} />
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
				<span class="skel" style="height: 50vh"></span>
			{/if}
		</div>
	{/if}

	<Dialog bind:open={syncOpen} title="Sync {app.name}">
		<p>Sync the Live State to Revision <code>{app.targetRevision}</code> ({short(app.targetSHA)}).</p>
		<div class="check">
			<input id="sync-prune" type="checkbox" bind:checked={syncPrune} />
			<label for="sync-prune"
				>Prune <span class="hint">Also delete containers of services that are no longer declared (volumes are kept).</span></label
			>
		</div>
		{#snippet actions()}
			<button onclick={() => (syncOpen = false)}>Cancel</button>
			<button class="primary" onclick={sync}><Icon name="sync" size={15} />Synchronize</button>
		{/snippet}
	</Dialog>

	<Dialog bind:open={delOpen} title="Delete {app.name}?">
		<p>The Application is removed from omegacd.</p>
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
	.crumbs { display: flex; align-items: center; gap: 0.35rem; color: var(--subtext0); margin-bottom: 0.8rem; flex-wrap: wrap; }
	.crumbs a { color: var(--subtext0); }
	.crumbs a:hover { color: var(--accent); }
	.crumbs [aria-current] { color: var(--text); font-weight: 550; }
	.hdr { margin-bottom: 1rem; display: grid; gap: 1.1rem; }
	.title { display: flex; align-items: center; gap: 0.6rem; flex-wrap: wrap; }
	.appicon { width: 40px; height: 40px; border-radius: 11px; display: grid; place-items: center; color: var(--accent); background: linear-gradient(135deg, color-mix(in srgb, var(--mauve) 22%, transparent), color-mix(in srgb, var(--blue) 16%, transparent)); border: 1px solid color-mix(in srgb, var(--accent) 25%, transparent); }
	h1 { overflow-wrap: anywhere; }
	.actions { display: flex; gap: 0.45rem; flex-wrap: wrap; }
	.panels { display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: 0.8rem; }
	.panel { display: grid; gap: 0.35rem; align-content: start; padding: 0.85rem 1rem; border-radius: var(--radius-lg); border: 1px solid var(--border); background: color-mix(in srgb, var(--bg) 55%, var(--card)); min-width: 0; }
	.plabel { font-size: 0.68rem; text-transform: uppercase; letter-spacing: 0.07em; color: var(--subtext0); font-weight: 650; }
	.meta { display: grid; grid-template-columns: repeat(auto-fill, minmax(190px, 1fr)); gap: 0.7rem 1.4rem; margin: 0; }
	.meta .wide { grid-column: span 2; }
	.meta dt { display: flex; align-items: center; gap: 0.3rem; font-size: 0.7rem; color: var(--subtext0); text-transform: uppercase; letter-spacing: 0.05em; font-weight: 600; }
	.meta dd { margin: 0.1rem 0 0; word-break: break-all; }
	.syncmsg { margin: 0; color: var(--subtext1); display: flex; gap: 0.4rem; align-items: flex-start; }
	.syncmsg :global(.icon) { margin-top: 0.15rem; color: var(--blue); }
	.banner, .error, .note { margin: 0 0 1rem; }
	.note { display: flex; gap: 0.5rem; align-items: flex-start; padding: 0.55rem 0.85rem; border-radius: var(--radius); color: var(--blue); background: color-mix(in srgb, var(--blue) 10%, transparent); border: 1px solid color-mix(in srgb, var(--blue) 35%, transparent); }
	.note strong { color: var(--text); }
	.banner :global(.icon), .error :global(.icon), .note :global(.icon) { margin-top: 0.15rem; }
	.mb { margin-bottom: 1rem; }
	.tabs { display: flex; gap: 0.25rem; border-bottom: 1px solid var(--border); margin-bottom: -1px; overflow-x: auto; }
	.tab { background: none; border: 1px solid transparent; border-bottom: 0; border-radius: var(--radius) var(--radius) 0 0; color: var(--subtext0); box-shadow: none; padding: 0.5rem 0.9rem; position: relative; }
	.tab:hover:not(:disabled) { background: color-mix(in srgb, var(--surface0) 50%, transparent); border-color: transparent; color: var(--text); }
	.tab[aria-selected='true'] { color: var(--text); background: var(--card); border-color: var(--border); }
	.tab[aria-selected='true']::after { content: ''; position: absolute; left: 0.6rem; right: 0.6rem; top: -1px; height: 2px; border-radius: 2px; background: var(--accent); }
	#panel { border-top-left-radius: 0; min-height: 200px; }
	@media (max-width: 640px) {
		.meta .wide { grid-column: auto; }
		.panels { grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 0.6rem; }
		.panel { padding: 0.65rem 0.8rem; }
		.actions { width: 100%; }
		.actions > :global(*) { flex: 1 1 auto; }
	}
</style>
