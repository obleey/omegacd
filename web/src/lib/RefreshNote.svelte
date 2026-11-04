<script lang="ts">
	// The answer to a Refresh ("Up to date", "New revision …"), shown for a moment next to the button.
	import { fly } from 'svelte/transition';
	import Icon from './Icon.svelte';
	import { live } from './live.svelte.ts';
	import { ms } from './status.ts';

	let { app }: { app: string } = $props();
	let n = $derived(live.notes[app]);
</script>

<span class="slot" aria-live="polite">
	{#if n}
		{#key n.id}
			<span class="note {n.tone}" in:fly={{ x: -6, duration: ms(220) }}>
				<Icon name={n.tone === 'green' ? 'check-circle' : n.tone === 'blue' ? 'arrow-circle' : 'alert'} size={13} />{n.text}
			</span>
		{/key}
	{/if}
</span>

<style>
	.slot { display: inline-flex; min-width: 0; }
	.note { display: inline-flex; align-items: center; gap: 0.3rem; font-size: 0.74rem; font-weight: 550; color: var(--c); white-space: nowrap; animation: hold 2.6s ease forwards; }
	.green { --c: var(--green); }
	.yellow { --c: var(--yellow); }
	.blue { --c: var(--blue); }
	/* fade out at the end of its life; the store removes it afterwards */
	@keyframes hold { 0%, 80% { opacity: 1; } 100% { opacity: 0; } }
	@media (prefers-reduced-motion: reduce) { .note { animation: none; } }
</style>
