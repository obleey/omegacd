<script lang="ts">
	import type { Snippet } from 'svelte';
	import Icon, { type IconName } from './Icon.svelte';

	let { icon, title, children }: { icon: IconName; title: string; children?: Snippet } = $props();
</script>

<div class="empty">
	<div class="art" aria-hidden="true">
		<span class="ring r2"></span><span class="ring r1"></span>
		<span class="disc"><Icon name={icon} size={30} /></span>
	</div>
	<h2>{title}</h2>
	{#if children}<div class="body">{@render children()}</div>{/if}
</div>

<style>
	.empty { display: grid; justify-items: center; text-align: center; padding: 2.2rem 1rem; gap: 0.4rem; }
	.art { position: relative; width: 120px; height: 120px; display: grid; place-items: center; margin-bottom: 0.6rem; }
	.ring { position: absolute; border-radius: 50%; border: 1px dashed color-mix(in srgb, var(--accent) 35%, transparent); }
	.r1 { inset: 14px; }
	.r2 { inset: 0; border-style: solid; border-color: color-mix(in srgb, var(--accent) 12%, transparent); }
	.disc { width: 64px; height: 64px; border-radius: 20px; display: grid; place-items: center; color: var(--accent); background: linear-gradient(135deg, color-mix(in srgb, var(--mauve) 22%, transparent), color-mix(in srgb, var(--blue) 18%, transparent)); border: 1px solid color-mix(in srgb, var(--accent) 30%, transparent); transform: rotate(-6deg); box-shadow: var(--shadow-md); }
	.disc :global(svg) { transform: rotate(6deg); }
	.body { color: var(--subtext0); max-width: 460px; }
	.body :global(p) { margin: 0.2rem 0 0.9rem; }
</style>
