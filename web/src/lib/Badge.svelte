<script lang="ts">
	// Status badge: icon + text always present, color is a secondary signal.
	import { fade } from 'svelte/transition';
	import Icon from './Icon.svelte';
	import { meta, ms } from './status.ts';

	let { status, label = '', size = 'md', text }: { status?: string; label?: string; size?: 'sm' | 'md' | 'lg'; text?: string } =
		$props();
	let m = $derived(meta(status));
</script>

<span class="badge {m[0]} {size}" title={label ? `${label}: ${status || 'Unknown'}` : undefined}>
	{#key status}
		<span class="in" in:fade={{ duration: ms(260) }}>
			<Icon name={m[1]} spin={m[2]} size={size === 'lg' ? 26 : size === 'sm' ? 13 : 14} />
			{#if label}<span class="sr-only">{label}:</span>{/if}
			<span class="txt">{text ?? (status || 'Unknown')}</span>
		</span>
	{/key}
</span>

<style>
	.badge {
		display: inline-flex;
		align-items: center;
		padding: 0.1rem 0.55rem 0.1rem 0.4rem;
		border-radius: 999px;
		font-size: 0.78rem;
		font-weight: 550;
		white-space: nowrap;
		border: 1px solid color-mix(in srgb, var(--c) 35%, transparent);
		background: color-mix(in srgb, var(--c) 12%, transparent);
		color: var(--c);
		transition: color 0.3s, background-color 0.3s, border-color 0.3s;
	}
	.in { display: inline-flex; align-items: center; gap: 0.32rem; }
	.sm { font-size: 0.72rem; padding: 0 0.45rem 0 0.32rem; }
	.lg { border: 0; background: none; padding: 0; font-size: 1.05rem; font-weight: 650; }
	.lg .in { gap: 0.55rem; }
	.green { --c: var(--green); }
	.yellow { --c: var(--yellow); }
	.red { --c: var(--red); }
	.blue { --c: var(--blue); }
	.overlay { --c: var(--overlay1); }
</style>
