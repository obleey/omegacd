<script lang="ts">
	// Compact Sync Policy summary: only the policies that are on.
	import type { AppView } from './api.ts';
	import Icon from './Icon.svelte';

	let { a }: { a: AppView } = $props();
</script>

{#if a.autoSync || a.prune || a.selfHeal}
	<span class="chips">
		{#if a.autoSync}<span class="pc" title="Auto-Sync on"><Icon name="sync" size={11} />auto</span>{/if}
		{#if a.selfHeal}<span class="pc" title="Self-Heal on{a.autoSync ? '' : ' (inactive without Auto-Sync)'}" class:off={!a.autoSync}><Icon name="heart" size={11} />heal</span>{/if}
		{#if a.prune}<span class="pc" title="Prune on"><Icon name="trash" size={11} />prune</span>{/if}
		{#if a.prune && a.pruneVolumes}<span class="pc vol" title="Prune volumes on: undeclared volumes are deleted">vol</span>{/if}
	</span>
{/if}

<style>
	.chips { display: inline-flex; gap: 0.25rem; flex-wrap: wrap; }
	.pc { display: inline-flex; align-items: center; gap: 0.2rem; padding: 0 0.4rem; border-radius: 5px; font-size: 0.7rem; font-weight: 600; color: var(--subtext1); background: color-mix(in srgb, var(--surface0) 70%, transparent); border: 1px solid var(--border); }
	.vol { color: var(--red); border-color: color-mix(in srgb, var(--red) 40%, var(--border)); }
	.off { opacity: 0.5; text-decoration: line-through; }
</style>
