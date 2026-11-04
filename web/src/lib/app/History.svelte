<script lang="ts">
	import { ago, short, type SyncOperation } from '../api.ts';
	import Empty from '../Empty.svelte';
	import Icon from '../Icon.svelte';
	import Badge from '../Badge.svelte';

	// onrollback is omitted for Child Applications: their Revision is pinned in Git, not rolled back here.
	let { ops, onrollback }: { ops: SyncOperation[] | null; onrollback?: (op: SyncOperation) => void } = $props();

	// The current deployment is the newest Succeeded op; rolling back to it is a no-op.
	let currentID = $derived(ops?.find((o) => o.phase === 'Succeeded')?.id);

	function duration(o: SyncOperation) {
		if (!o.finishedAt) return '';
		const s = Math.round((new Date(o.finishedAt).getTime() - new Date(o.startedAt).getTime()) / 1000);
		return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${s % 60}s`;
	}
</script>

{#if !ops}
	<div class="loading" aria-busy="true" aria-label="Loading history">
		{#each [0, 1, 2, 3] as i (i)}<span class="skel" style="height: 38px"></span>{/each}
	</div>
{:else if ops.length === 0}
	<Empty icon="clock" title="No Sync Operations yet">
		<p>Every Sync and Rollback of this Application is recorded here.</p>
	</Empty>
{:else}
	<div class="scroll">
		<table>
			<thead>
				<tr><th>Revision</th><th>Trigger</th><th>Phase</th><th>Message</th><th>Started</th><th><span class="sr-only">Actions</span></th></tr>
			</thead>
			<tbody>
				{#each ops as o (o.id)}
					<tr>
						<td class="mono" title={o.revision}>{short(o.revision)}</td>
						<td>{o.trigger}</td>
						<td><Badge status={o.phase} label="Phase" /></td>
						<td class="msg small">{o.message || '—'}</td>
						<td class="small nowrap" title={new Date(o.startedAt).toLocaleString()}>
							{ago(o.startedAt)}
							{#if duration(o)}<span class="muted">· {duration(o)}</span>{/if}
						</td>
						<td class="right">
							{#if o.id === currentID}
								<span class="tag">Current</span>
							{:else if o.phase === 'Succeeded' && onrollback}
								<button class="sm" onclick={() => onrollback(o)} aria-label="Rollback to {short(o.revision)}"><Icon name="undo" size={14} />Rollback</button>
							{/if}
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>
{/if}

<style>
	.loading { display: grid; gap: 0.5rem; }
	tbody tr:hover { background: color-mix(in srgb, var(--surface0) 35%, transparent); }
	.scroll { overflow-x: auto; }
	.msg { max-width: 420px; white-space: pre-wrap; word-break: break-word; }
	.nowrap { white-space: nowrap; }
	.right { text-align: right; }
</style>
