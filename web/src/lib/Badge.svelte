<script lang="ts">
	// Status badge: text always present, color is a secondary signal.
	let { status, label = '' }: { status?: string; label?: string } = $props();

	const tone: Record<string, string> = {
		Synced: 'green', Healthy: 'green', Connected: 'green', Succeeded: 'green', running: 'green',
		OutOfSync: 'yellow', Progressing: 'yellow', Running: 'yellow', restarting: 'yellow', created: 'yellow', paused: 'yellow',
		Degraded: 'red', Failed: 'red', Disconnected: 'red', dead: 'red',
		Unknown: 'overlay', Missing: 'overlay', exited: 'overlay', removing: 'overlay'
	};
	const icon: Record<string, string> = { green: '●', yellow: '◐', red: '✕', overlay: '○' };
	let t = $derived(tone[status ?? ''] ?? 'overlay');
</script>

<span class="badge {t}" title={label ? `${label}: ${status ?? 'Unknown'}` : undefined}>
	<span aria-hidden="true">{icon[t]}</span>
	{#if label}<span class="sr-only">{label}:</span>{/if}
	{status || 'Unknown'}
</span>

<style>
	.badge {
		display: inline-flex;
		align-items: center;
		gap: 0.3rem;
		padding: 0.05rem 0.5rem;
		border-radius: 999px;
		font-size: 0.78rem;
		font-weight: 500;
		white-space: nowrap;
		border: 1px solid color-mix(in srgb, var(--c) 45%, transparent);
		background: color-mix(in srgb, var(--c) 14%, transparent);
		color: var(--c);
	}
	.badge span[aria-hidden] { font-size: 0.65rem; }
	.green { --c: var(--green); }
	.yellow { --c: var(--yellow); }
	.red { --c: var(--red); }
	.overlay { --c: var(--overlay1); }
</style>
