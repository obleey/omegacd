<script lang="ts">
	import { goto } from '$app/navigation';
	import { ago, enc, pendingChildren, short, type AppView, type PendingChild } from '../api.ts';
	import Badge from '../Badge.svelte';
	import Icon from '../Icon.svelte';
	import Empty from '../Empty.svelte';
	import Graph, { type GNode } from './Graph.svelte';
	import { shownSync } from '../status.ts';

	let { app, apps }: { app: AppView; apps: AppView[] } = $props();

	let mode = $state<'graph' | 'list'>(matchMedia('(max-width: 640px)').matches ? 'list' : 'graph');

	// Children declared in Git but not created yet: the App of Apps' next Sync creates them
	let pending = $state<PendingChild[]>([]);
	$effect(() => {
		const key = `${app.targetSHA}:${apps.length}`;
		pendingChildren(app.name)
			.then((p) => key && (pending = p))
			.catch(() => (pending = []));
	});
	let nodes = $derived<GNode[]>([
		{ id: app.name, icon: 'apps', kind: 'App of Apps', name: app.name, status: [['Sync Status', shownSync(app)], ['Health Status', app.health]] },
		...apps.map(
			(a): GNode => ({
				id: `a:${a.name}`,
				parent: app.name,
				icon: a.kind === 'apps' ? 'apps' : 'app',
				kind: 'Child Application',
				name: a.name,
				sub: `${a.host} · ${a.targetRevision} ${short(a.targetSHA)}`,
				status: [['Sync Status', shownSync(a)], ['Health Status', a.health]],
				tag: a.notInGit ? 'not in Git' : undefined,
				dim: a.notInGit,
				onclick: () => goto(`/apps/${enc(a.name)}`),
				action: `Open ${a.name}`
			})
		),
		...pending.map(
			(p): GNode => ({
				id: `p:${p.name}`,
				parent: app.name,
				icon: 'app',
				kind: 'Child Application',
				name: p.name,
				sub: `${p.host} · Sync ${app.name} to create`,
				status: [['Sync Status', 'OutOfSync'], ['Health Status', 'Missing']],
				tag: 'pending',
				dim: true
			})
		)
	]);
</script>

{#if apps.length === 0 && pending.length === 0}
	<Empty icon="apps" title="No Child Applications yet">
		<p>Sync to create them from the Application definitions in Git.</p>
	</Empty>
{:else}
	<div class="bar">
		<span class="muted small">{apps.length} Child Application{apps.length === 1 ? '' : 's'}{#if pending.length} · {pending.length} pending{/if}</span>
		<span class="spacer"></span>
		<div class="seg" role="group" aria-label="Child Applications view">
			<button aria-pressed={mode === 'graph'} onclick={() => (mode = 'graph')}><Icon name="tree" size={15} />Tree</button>
			<button aria-pressed={mode === 'list'} onclick={() => (mode = 'list')}><Icon name="list" size={15} />List</button>
		</div>
	</div>
	{#if mode === 'graph'}
		<Graph {nodes} label="Child Applications of {app.name}" />
	{:else}
		<div class="scroll-x">
			<table>
				<thead>
					<tr>
						<th>Child Application</th>
						<th>Host</th>
						<th>Sync Status</th>
						<th>Health Status</th>
						<th>Last Sync</th>
					</tr>
				</thead>
				<tbody>
					{#each apps as a (a.name)}
						{@const op = a.lastOperation}
						<tr>
							<td>
								<a class="name" href="/apps/{enc(a.name)}">{a.name}</a>
								{#if a.kind === 'apps'}<span class="tag">App of Apps</span>{/if}
								{#if a.notInGit}<span class="tag warn">not in Git</span>{/if}
							</td>
							<td class="mono small">{a.host}</td>
							<td><Badge status={shownSync(a)} label="Sync Status" /></td>
							<td><Badge status={a.health} label="Health Status" /></td>
							<td class="small">
								{#if op}
									<Badge status={op.phase} size="sm" label="Last Sync Operation" />
									<span class="muted">{op.trigger} · {ago(op.finishedAt || op.startedAt)}</span>
								{:else}
									<span class="muted">Never</span>
								{/if}
							</td>
						</tr>
					{/each}
					{#each pending as p (p.name)}
						<tr class="pending">
							<td><span class="name">{p.name}</span> <span class="tag">pending</span></td>
							<td class="mono small">{p.host}</td>
							<td><Badge status="OutOfSync" label="Sync Status" /></td>
							<td><Badge status="Missing" label="Health Status" /></td>
							<td class="small muted">Sync {app.name} to create</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	{/if}
{/if}

<style>
	.bar { display: flex; align-items: center; gap: 0.6rem; flex-wrap: wrap; margin-bottom: 0.6rem; }
	.name { font-weight: 600; }
	.pending { opacity: 0.65; }
</style>
