<script lang="ts">
	import { ago, enc, type AppView } from '../api';
	import Badge from '../Badge.svelte';

	let { apps }: { apps: AppView[] } = $props();
</script>

{#if apps.length === 0}
	<p class="muted">No Child Applications yet. Sync to create them from the Application definitions in Git.</p>
{:else}
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
					<td class="mono small">{a.host || 'local'}</td>
					<td><Badge status={a.syncStatus} label="Sync Status" /></td>
					<td><Badge status={a.health} label="Health Status" /></td>
					<td class="small">
						{#if op}
							<Badge status={op.phase} />
							<span class="muted">{op.trigger} · {ago(op.finishedAt || op.startedAt)}</span>
						{:else}
							<span class="muted">Never</span>
						{/if}
					</td>
				</tr>
			{/each}
		</tbody>
	</table>
{/if}

<style>
	.name { font-weight: 600; }
	tbody tr:last-child td { border-bottom: 0; }
</style>
