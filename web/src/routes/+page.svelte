<script lang="ts">
	import { onMount } from 'svelte';
	import { flip } from 'svelte/animate';
	import { fly, slide } from 'svelte/transition';
	import { ago, enc, errMsg, short, type AppView } from '#lib/api.ts';
	import { live, loadApps } from '#lib/live.svelte.ts';
	import Badge from '#lib/Badge.svelte';
	import Icon from '#lib/Icon.svelte';
	import AppTile from '#lib/AppTile.svelte';
	import Empty from '#lib/Empty.svelte';
	import { refreshApp, syncApp } from '#lib/actions.ts';
	import PolicyChips from '#lib/PolicyChips.svelte';
	import { isSyncing, meta, ms, shownSync } from '#lib/status.ts';

	let filter = $state('');
	let error = $state('');
	let busy = $state<Record<string, boolean>>({});

	// Tiles or list, remembered per browser.
	const VIEW_KEY = 'dockercd.appsView';
	let view = $state<'tiles' | 'list'>('tiles');
	onMount(() => {
		try {
			if (localStorage.getItem(VIEW_KEY) === 'list') view = 'list';
		} catch {
			/* storage unavailable: keep the default */
		}
		loadApps().catch((e) => (error = errMsg(e)));
	});
	function setView(v: typeof view) {
		view = v;
		try {
			localStorage.setItem(VIEW_KEY, v);
		} catch {
			/* not remembered */
		}
	}

	// Status filters: OR within Sync Status / Health Status, AND across them.
	const syncKeys = ['Synced', 'OutOfSync', 'Syncing', 'Unknown'];
	const healthKeys = ['Healthy', 'Progressing', 'Degraded', 'Missing'];
	let selSync = $state<string[]>([]);
	let selHealth = $state<string[]>([]);
	const toggle = (list: string[], k: string) => (list.includes(k) ? list.filter((x) => x !== k) : [...list, k]);
	let all = $derived(Object.values(live.apps));
	let counts = $derived.by(() => {
		const c: Record<string, number> = {};
		for (const a of all) {
			c[shownSync(a)] = (c[shownSync(a)] ?? 0) + 1;
			c[a.health] = (c[a.health] ?? 0) + 1;
		}
		return c;
	});
	let filtering = $derived(!!filter.trim() || selSync.length > 0 || selHealth.length > 0);

	let collapsed = $state<Record<string, boolean>>({});
	let q = $derived(filter.trim().toLowerCase());
	const matches = (a: AppView) =>
		(!q || [a.name, a.host, a.repoURL, a.path, a.syncStatus, a.health].some((s) => s?.toLowerCase().includes(q))) &&
		(!selSync.length || selSync.includes(shownSync(a))) &&
		(!selHealth.length || selHealth.includes(a.health));

	// Child Applications grouped by their App of Apps; a child whose parent is gone is shown top-level.
	let kidsOf = $derived.by(() => {
		const m: Record<string, AppView[]> = {};
		for (const a of all) if (a.parent && live.apps[a.parent]) (m[a.parent] ??= []).push(a);
		for (const k of Object.values(m)) k.sort((x, y) => x.name.localeCompare(y.name));
		return m;
	});
	let top = $derived(all.filter((a) => !a.parent || !live.apps[a.parent]).sort((a, b) => a.name.localeCompare(b.name)));

	type Row = { a: AppView; depth: number; kids: number; open: boolean };
	// A row is shown when it or a descendant matches the filter; filtering expands every parent.
	function rowsOf(a: AppView, depth: number): Row[] {
		const kids = kidsOf[a.name] ?? [];
		const open = filtering || !collapsed[a.name];
		const sub = kids.flatMap((k) => rowsOf(k, depth + 1));
		if (!matches(a) && !sub.length) return [];
		return [{ a, depth, kids: kids.length, open }, ...(open ? sub : [])];
	}
	let rows = $derived(top.flatMap((a) => rowsOf(a, 0)));

	// Tiles: each App of Apps is a group holding all its matching descendants.
	type Group = { a: AppView; kids: AppView[]; total: number; open: boolean };
	let groups = $derived(
		top.flatMap((a): Group[] => {
			const desc = rowsOf(a, 0).slice(1).map((r) => r.a);
			if (!matches(a) && !desc.length) return [];
			const total = kidsOf[a.name]?.length ?? 0;
			return [{ a, kids: desc, total, open: filtering || !collapsed[a.name] }];
		})
	);

	async function rowAction(name: string, fn: (n: string) => Promise<unknown>) {
		busy[name] = true;
		await fn(name);
		busy[name] = false;
	}
</script>

<svelte:head><title>Applications · dockercd</title></svelte:head>

<div class="head">
	<div>
		<h1>Applications</h1>
		<p class="muted small sub">{all.length} Application{all.length === 1 ? '' : 's'} managed by dockercd</p>
	</div>
	<span class="spacer"></span>
	<div class="search">
		<Icon name="search" size={15} />
		<label class="sr-only" for="filter">Filter Applications</label>
		<input id="filter" type="search" placeholder="Filter by name, Host, Repository…" bind:value={filter} />
	</div>
	<div class="seg" role="group" aria-label="View">
		<button aria-pressed={view === 'tiles'} onclick={() => setView('tiles')} title="Tiles"><Icon name="grid" size={15} /><span class="sr-only">Tiles</span></button>
		<button aria-pressed={view === 'list'} onclick={() => setView('list')} title="List"><Icon name="list" size={15} /><span class="sr-only">List</span></button>
	</div>
	<a class="btn primary" href="/apps/new"><Icon name="plus" size={15} />New Application</a>
</div>

{#snippet chips(title: string, keys: string[], sel: string[], set: (v: string[]) => void)}
	<div class="sgroup" role="group" aria-label={title}>
		<span class="slabel">{title}</span>
		{#each keys as k (k)}
			{@const m = meta(k)}
			<button class="chip {m[0]}" aria-pressed={sel.includes(k)} disabled={!counts[k] && !sel.includes(k)} onclick={() => set(toggle(sel, k))}>
				<Icon name={m[1]} size={14} spin={m[2] && !!counts[k]} />
				<span>{k}</span>
				<span class="count num">{counts[k] ?? 0}</span>
			</button>
		{/each}
	</div>
{/snippet}

{#if all.length}
	<div class="summary" aria-label="Filter by status">
		{@render chips('Sync Status', syncKeys, selSync, (v) => (selSync = v))}
		{@render chips('Health Status', healthKeys, selHealth, (v) => (selHealth = v))}
		{#if filtering}
			<button class="ghost sm" onclick={() => ((selSync = []), (selHealth = []), (filter = ''))}><Icon name="x" size={13} />Clear filters</button>
		{/if}
	</div>
{/if}

{#if error}<p class="error" role="alert">{error}</p>{/if}

{#if !live.loaded && !error}
	<div class="tiles" aria-busy="true" aria-label="Loading Applications">
		{#each [0, 1, 2, 3, 4, 5] as i (i)}
			<div class="card skel-tile">
				<span class="skel" style="width: 55%; height: 18px"></span>
				<span class="skel" style="width: 70%; height: 20px"></span>
				<span class="skel" style="height: 64px"></span>
				<span class="skel" style="width: 40%; height: 26px"></span>
			</div>
		{/each}
	</div>
{:else if all.length === 0}
	<div class="card">
		<Empty icon="app" title="No Applications yet">
			<p>An Application deploys a Compose project from a path in a Git Repository to a Host.</p>
			<a class="btn primary" href="/apps/new"><Icon name="plus" size={15} />New Application</a>
		</Empty>
	</div>
{:else if rows.length === 0}
	<div class="card">
		<Empty icon="search" title="No matching Applications">
			<p>Nothing matches the current filters.</p>
			<button onclick={() => ((selSync = []), (selHealth = []), (filter = ''))}>Clear filters</button>
		</Empty>
	</div>
{:else if view === 'tiles'}
	<div class="tiles">
		{#each groups as g, i (g.a.name)}
			<div
				class="cell"
				class:group={g.a.kind === 'apps' && g.total > 0}
				in:fly|global={{ y: 16, duration: ms(420), delay: ms(Math.min(i, 10) * 45) }}
				animate:flip={{ duration: ms(350) }}
			>
				{#if g.a.kind === 'apps' && g.total > 0}
					<div class="ghead">
						<button
							class="ghost sm"
							aria-expanded={g.open}
							disabled={filtering}
							onclick={() => (collapsed[g.a.name] = g.open)}
						>
							<span class="chev" class:open={g.open}><Icon name="chevron" size={14} /></span>
							{g.open ? 'Hide' : 'Show'} {g.total} Child Application{g.total === 1 ? '' : 's'}
						</button>
						<span class="muted small">of App of Apps <strong>{g.a.name}</strong></span>
					</div>
					<div class="tiles inner">
						<AppTile a={g.a} kids={g.total} />
						{#if g.open}
							{#each g.kids as k, j (k.name)}
								<div
									class="kid"
									in:fly={{ x: -14, duration: ms(320), delay: ms(j * 50) }}
									out:fly={{ x: -14, duration: ms(180) }}
									animate:flip={{ duration: ms(300) }}
								>
									<AppTile a={k} kids={kidsOf[k.name]?.length} />
								</div>
							{/each}
						{/if}
					</div>
				{:else}
					<AppTile a={g.a} kids={g.total} />
				{/if}
			</div>
		{/each}
	</div>
{:else}
	<div class="card flush scroll-x">
		<table>
			<thead>
				<tr>
					<th>Application</th>
					<th>Host</th>
					<th>Revision</th>
					<th>Sync Status</th>
					<th>Health Status</th>
					<th>Last Sync</th>
					<th><span class="sr-only">Actions</span></th>
				</tr>
			</thead>
			<tbody>
				{#each rows as { a, depth, kids, open } (a.name)}
					{@const op = a.lastOperation}
					<tr class:child={depth > 0} animate:flip={{ duration: ms(250) }} in:slide={{ duration: ms(200) }}>
						<td style:padding-left="{0.7 + depth * 1.6}rem">
							<div class="namecell">
								{#if kids}
									<button
										class="ghost toggle"
										aria-expanded={open}
										aria-label="{open ? 'Hide' : 'Show'} Child Applications of {a.name}"
										disabled={filtering}
										onclick={() => (collapsed[a.name] = open)}><span class="chev" class:open><Icon name="chevron" size={14} /></span></button
									>
								{:else if depth > 0}
									<span class="muted" aria-hidden="true">↳</span>
								{/if}
								<a class="name" href="/apps/{enc(a.name)}">{a.name}</a>
								{#if a.kind === 'apps'}<span class="tag">App of Apps</span>{/if}
								{#if a.notInGit}<span class="tag warn">not in Git</span>{/if}
							</div>
							<div class="muted small mono trunc" title="{a.repoURL} {a.path}">
								{a.repoURL.replace(/^https?:\/\//, '')}{a.path && a.path !== '.' ? ` · ${a.path}` : ''}
							</div>
						</td>
						<td class="mono small">{a.kind === 'apps' ? '—' : a.host || 'local'}</td>
						<td class="mono small">
							{a.targetRevision}
							<div class="muted">{short(a.targetSHA)}</div>
						</td>
						<td>
							<Badge status={shownSync(a)} label="Sync Status" />
							<div class="pol"><PolicyChips {a} /></div>
						</td>
						<td><Badge status={a.health} label="Health Status" /></td>
						<td class="small">
							{#if op}
								<Badge status={op.phase} size="sm" label="Last Sync Operation" />
								<div class="muted">{op.trigger} · {ago(op.finishedAt || op.startedAt)}</div>
							{:else}
								<span class="muted">Never</span>
							{/if}
						</td>
						<td class="actions">
							<button class="sm" onclick={() => rowAction(a.name, syncApp)} disabled={busy[a.name] || isSyncing(a)} aria-label="Sync {a.name}"
								><Icon name="sync" size={14} spin={isSyncing(a)} />{isSyncing(a) ? 'Syncing' : 'Sync'}</button
							>
							<button class="sm ghost icon-btn" onclick={() => rowAction(a.name, refreshApp)} disabled={busy[a.name]} aria-label="Refresh {a.name}" title="Refresh"
								><Icon name="refresh" size={14} /></button
							>
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>
{/if}

<style>
	.head { display: flex; gap: 0.7rem; align-items: center; flex-wrap: wrap; margin-bottom: 1.1rem; }
	.sub { margin: 0.15rem 0 0; }
	.search { position: relative; width: 280px; max-width: 100%; }
	.search :global(.icon) { position: absolute; left: 0.65rem; top: 50%; transform: translateY(-50%); color: var(--overlay1); pointer-events: none; }
	.search input { padding-left: 2.1rem; background: var(--card); }
	.summary { display: flex; flex-wrap: wrap; gap: 0.6rem 1.4rem; align-items: center; padding: 0.6rem 0.8rem; margin-bottom: 1.2rem; background: var(--card); border: 1px solid var(--border); border-radius: var(--radius-lg); box-shadow: var(--shadow); }
	.sgroup { display: flex; flex-wrap: wrap; align-items: center; gap: 0.35rem; }
	.slabel { font-size: 0.7rem; text-transform: uppercase; letter-spacing: 0.06em; color: var(--subtext0); font-weight: 600; margin-right: 0.25rem; }
	.chip { --c: var(--overlay1); box-shadow: none; padding: 0.22rem 0.35rem 0.22rem 0.55rem; font-size: 0.8rem; border-radius: 999px; background: transparent; border-color: transparent; color: var(--subtext1); }
	.chip :global(.icon) { color: var(--c); }
	.chip:hover:not(:disabled) { background: color-mix(in srgb, var(--c) 10%, transparent); border-color: color-mix(in srgb, var(--c) 30%, transparent); }
	.chip[aria-pressed='true'] { background: color-mix(in srgb, var(--c) 16%, transparent); border-color: color-mix(in srgb, var(--c) 55%, transparent); color: var(--text); }
	.chip:disabled { opacity: 0.4; }
	.count { min-width: 1.5rem; padding: 0 0.4rem; border-radius: 999px; background: color-mix(in srgb, var(--surface0) 80%, transparent); font-size: 0.75rem; font-weight: 650; text-align: center; }
	.chip.green { --c: var(--green); }
	.chip.yellow { --c: var(--yellow); }
	.chip.red { --c: var(--red); }
	.chip.blue { --c: var(--blue); }
	.tiles { display: grid; grid-template-columns: repeat(auto-fill, minmax(min(310px, 100%), 1fr)); gap: 1rem; align-items: stretch; }
	.cell { min-width: 0; }
	.cell.group { grid-column: 1 / -1; padding: 0.6rem 0.9rem 1rem; border: 1px dashed color-mix(in srgb, var(--lavender) 35%, var(--border)); border-radius: calc(var(--radius-lg) + 4px); background: color-mix(in srgb, var(--lavender) 4%, transparent); }
	.ghead { display: flex; align-items: center; gap: 0.5rem; flex-wrap: wrap; margin-bottom: 0.6rem; }
	.ghead button { margin-left: -0.5rem; }
	.inner { gap: 0.9rem; }
	.kid { min-width: 0; position: relative; }
	.chev { display: inline-flex; transition: transform 0.25s var(--ease); }
	.chev.open { transform: rotate(90deg); }
	.skel-tile { display: grid; gap: 0.8rem; min-height: 210px; }
	.flush { padding: 0; }
	.namecell { display: flex; align-items: center; gap: 0.4rem; flex-wrap: wrap; }
	.name { font-weight: 620; font-size: 0.95rem; color: var(--text); }
	.toggle { padding: 0.1rem; color: var(--subtext0); }
	.child { background: color-mix(in srgb, var(--crust) 25%, transparent); }
	.trunc { max-width: 360px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
	.pol { margin-top: 0.3rem; }
	.actions { text-align: right; white-space: nowrap; }
	tbody tr:hover { background: color-mix(in srgb, var(--surface0) 35%, transparent); }
	@media (max-width: 640px) {
		.search { width: 100%; order: 5; }
		.summary { gap: 0.7rem; }
		.slabel { flex-basis: 100%; }
		.head h1 { font-size: 1.35rem; }
	}
</style>
