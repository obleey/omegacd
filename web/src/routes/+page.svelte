<script lang="ts">
	import { onMount } from 'svelte';
	import { api, ago, enc, errMsg, short } from '#lib/api.ts';
	import { live, loadApps } from '#lib/live.svelte.ts';
	import Badge from '#lib/Badge.svelte';

	let filter = $state('');
	let error = $state('');
	let syncing = $state<Record<string, boolean>>({});

	onMount(() => {
		loadApps().catch((e) => (error = errMsg(e)));
	});

	let apps = $derived(
		Object.values(live.apps)
			.filter((a) => {
				const q = filter.trim().toLowerCase();
				return !q || [a.name, a.host, a.repoURL, a.path, a.syncStatus, a.health].some((s) => s?.toLowerCase().includes(q));
			})
			.sort((a, b) => a.name.localeCompare(b.name))
	);

	async function sync(name: string) {
		error = '';
		syncing[name] = true;
		try {
			await api('POST', `/apps/${enc(name)}/sync`);
		} catch (e) {
			error = `${name}: ${errMsg(e)}`;
		} finally {
			syncing[name] = false;
		}
	}
</script>

<svelte:head><title>Applications · dockercd</title></svelte:head>

<div class="row head">
	<h1>Applications</h1>
	<span class="spacer"></span>
	<label class="sr-only" for="filter">Filter Applications</label>
	<input id="filter" class="filter" type="search" placeholder="Filter…" bind:value={filter} />
	<a class="btn primary" href="/apps/new">New Application</a>
</div>

{#if error}<p class="error" role="alert">{error}</p>{/if}

{#if !live.loaded && !error}
	<p class="muted">Loading…</p>
{:else if Object.keys(live.apps).length === 0}
	<div class="card empty">
		<h2>No Applications yet</h2>
		<p class="muted">An Application deploys a Compose project from a path in a Git Repository.</p>
		<a class="btn primary" href="/apps/new">New Application</a>
	</div>
{:else if apps.length === 0}
	<p class="muted">No Applications match “{filter}”.</p>
{:else}
	<div class="card flush">
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
				{#each apps as a (a.name)}
					{@const op = a.lastOperation}
					<tr>
						<td>
							<a class="name" href="/apps/{enc(a.name)}">{a.name}</a>
							<div class="muted small mono trunc" title="{a.repoURL} {a.path}">
								{a.repoURL.replace(/^https?:\/\//, '')}{a.path && a.path !== '.' ? ` · ${a.path}` : ''}
							</div>
						</td>
						<td class="mono small">{a.host || 'local'}</td>
						<td class="mono small">
							{a.targetRevision}
							<div class="muted">{short(a.targetSHA)}</div>
						</td>
						<td>
							<Badge status={a.syncStatus} label="Sync Status" />
							{#if a.autoSync}<div class="muted small">Auto-Sync</div>{/if}
						</td>
						<td><Badge status={a.health} label="Health Status" /></td>
						<td class="small">
							{#if op}
								<Badge status={op.phase} />
								<div class="muted">{op.trigger} · {ago(op.finishedAt || op.startedAt)}</div>
							{:else}
								<span class="muted">Never</span>
							{/if}
						</td>
						<td class="actions">
							<button
								onclick={() => sync(a.name)}
								disabled={syncing[a.name] || op?.phase === 'Running'}
								aria-label="Sync {a.name}">{op?.phase === 'Running' ? 'Syncing…' : 'Sync'}</button
							>
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>
{/if}

<style>
	.head { margin-bottom: 1.2rem; }
	.filter { width: 220px; }
	.flush { padding: 0; overflow-x: auto; }
	.name { font-weight: 600; font-size: 0.95rem; }
	.trunc { max-width: 360px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
	.actions { text-align: right; }
	tbody tr:last-child td { border-bottom: 0; }
	tbody tr:hover { background: color-mix(in srgb, var(--surface0) 40%, transparent); }
	.empty { text-align: center; padding: 2.5rem; }
	.empty p { margin: 0.5rem 0 1.2rem; }
</style>
