<script lang="ts">
	// An Application's or a Host's badges about the Host (M5-03, Q39), each with its tooltip.
	import Badge from './Badge.svelte';
	import type { HostBadge } from './badges.ts';

	let { badges }: { badges: HostBadge[] } = $props();
</script>

{#if badges.length}
	<span class="hbs">
		{#each badges as b (b.status)}
			<span class="hb" title={b.title}><Badge status={b.status} text={b.text} size="sm" /><span class="sr-only">: {b.title}</span></span>
		{/each}
	</span>
{/if}

<style>
	/* in the normal font, also beside a Host's name set in --mono */
	.hbs { display: inline-flex; flex-wrap: wrap; gap: 0.3rem; font-family: var(--sans); }
	.hb { display: inline-flex; }
</style>
