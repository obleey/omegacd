<script lang="ts">
	import { flip } from 'svelte/animate';
	import { fly, fade } from 'svelte/transition';
	import Icon from './Icon.svelte';
	import { toasts, dismiss } from './toast.svelte.ts';
	import { ms } from './status.ts';

	const icon = { green: 'check-circle', red: 'x-circle', blue: 'sync' } as const;
</script>

<div class="toasts" role="status" aria-live="polite">
	{#each toasts as t (t.id)}
		<div
			class="toast {t.tone}"
			in:fly={{ x: 40, duration: ms(320) }}
			out:fade={{ duration: ms(200) }}
			animate:flip={{ duration: ms(250) }}
		>
			<Icon name={icon[t.tone]} size={18} spin={t.tone === 'blue'} />
			<div class="txt">
				<strong>{#if t.href}<a href={t.href}>{t.title}</a>{:else}{t.title}{/if}</strong>
				{#if t.body}<p>{t.body}</p>{/if}
			</div>
			<button class="ghost x" onclick={() => dismiss(t.id)} aria-label="Dismiss"><Icon name="x" size={14} /></button>
			<span class="timer" style:animation-duration="{t.tone === 'red' ? 9 : 5}s"></span>
		</div>
	{/each}
</div>

<style>
	.toasts { position: fixed; right: 1rem; bottom: 1rem; z-index: 50; display: grid; gap: 0.6rem; width: min(360px, calc(100vw - 2rem)); }
	.toast {
		--c: var(--blue);
		position: relative;
		overflow: hidden;
		display: flex;
		gap: 0.7rem;
		align-items: flex-start;
		padding: 0.75rem 0.6rem 0.8rem 0.9rem;
		background: var(--elevated);
		border: 1px solid var(--border);
		border-left: 3px solid var(--c);
		border-radius: var(--radius-lg);
		box-shadow: var(--shadow-lg);
	}
	.toast > :global(.icon) { color: var(--c); margin-top: 0.1rem; }
	.green { --c: var(--green); }
	.red { --c: var(--red); }
	.txt { flex: 1; min-width: 0; }
	.txt strong { font-weight: 600; }
	.txt a { color: var(--text); }
	.txt p { margin: 0.15rem 0 0; color: var(--subtext0); font-size: 0.85rem; overflow-wrap: anywhere; }
	.x { padding: 0.2rem; color: var(--subtext0); }
	.timer { position: absolute; left: 0; bottom: 0; height: 2px; width: 100%; background: var(--c); opacity: 0.5; transform-origin: left; animation: shrink linear forwards; }
	@keyframes shrink { from { transform: scaleX(1); } to { transform: scaleX(0); } }
</style>
