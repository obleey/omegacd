<script lang="ts">
	import { slide } from 'svelte/transition';
	import { api, enc, type AppView } from '../api.ts';
	import { putApp } from '../live.svelte.ts';
	import { toast, toastError } from '../toast.svelte.ts';
	import { ms } from '../status.ts';
	import Dialog from '../Dialog.svelte';

	let { app }: { app: AppView } = $props();

	type Field = 'autoSync' | 'prune' | 'selfHeal' | 'pruneVolumes';
	const names: Record<Field, string> = { autoSync: 'Auto-Sync', prune: 'Prune', selfHeal: 'Self-Heal', pruneVolumes: 'Prune volumes' };
	let askVolumes = $state(false);
	let saving = $state<Field | ''>('');
	// Child Application settings are owned by Git.
	let readonly = $derived(!!app.parent);

	// Optimistic: flip locally, send the full Application, roll back on error.
	async function set(f: Field, v: boolean) {
		const before = app;
		// Prune volumes is a sub-option of Prune: turning Prune off turns it off too
		const patch = f === 'prune' && !v ? { prune: false, pruneVolumes: false } : { [f]: v };
		saving = f;
		putApp({ ...app, ...patch });
		try {
			putApp(await api<AppView>('PUT', `/apps/${enc(app.name)}`, { ...before, ...patch }));
			toast('green', `${names[f]} ${v ? 'on' : 'off'}`, app.name);
		} catch (e) {
			putApp(before);
			toastError(e, `Could not change ${names[f]}`);
		} finally {
			saving = '';
		}
	}
</script>

{#snippet sw(f: Field, label: string, hint: string, disabled = false, toggle: () => unknown = () => set(f, !app[f]))}
	<div class="opt" class:danger={f === 'pruneVolumes'}>
		<button
			type="button"
			role="switch"
			id="sp-{f}"
			class="switch"
			aria-checked={app[f]}
			aria-labelledby="sp-{f}-name"
			aria-describedby="sp-{f}-hint"
			disabled={readonly || disabled || !!saving}
			onclick={toggle}
		>
			<span class="knob"></span>
		</button>
		<label for="sp-{f}">
			<span id="sp-{f}-name">{label}</span>
			<span class="hint" id="sp-{f}-hint">{hint}</span>
		</label>
	</div>
{/snippet}

<section class="policy" aria-labelledby="sp-title">
	<h3 id="sp-title">Sync Policy</h3>
	<div class="opts">
		{@render sw('autoSync', 'Auto-Sync', app.rolledBack && !app.autoSync ? 'Auto-Sync was turned off by a Rollback' : 'Sync when the Repository moves to a new Revision')}
		{@render sw('selfHeal', 'Self-Heal', app.autoSync ? 'Sync when the Live State drifts' : 'requires Auto-Sync', !app.autoSync)}
		<div class="prune">
			{@render sw('prune', 'Prune', 'Delete what is no longer declared')}
			{#if app.prune && app.kind !== 'apps'}
				<div class="nested" transition:slide={{ duration: ms(180) }}>
					{@render sw('pruneVolumes', '…also volumes', 'Deletes volumes no longer declared — data is lost', false, () =>
						app.pruneVolumes ? set('pruneVolumes', false) : (askVolumes = true)
					)}
				</div>
			{/if}
		</div>
	</div>
	{#if readonly}
		<p class="note small">Managed in Git by <a href="/apps/{enc(app.parent ?? '')}">{app.parent}</a>.</p>
	{/if}
	{#if app.prune && !readonly}
		<p class="note small" transition:slide={{ duration: ms(200) }}>
			Containers of services no longer declared will be deleted on the next Sync ({app.pruneVolumes
				? 'and volumes no longer declared, with their data'
				: 'volumes are kept'}).
		</p>
	{/if}
</section>

<Dialog bind:open={askVolumes} title="Prune volumes of {app.name}?">
	<p>Volumes this Application no longer declares will be <strong>deleted with all their data</strong> on the next Sync (unless a container still uses them).</p>
	{#snippet actions()}
		<button onclick={() => (askVolumes = false)}>Cancel</button>
		<button class="danger solid" onclick={() => ((askVolumes = false), set('pruneVolumes', true))}>Prune volumes</button>
	{/snippet}
</Dialog>

<style>
	.policy { display: grid; gap: 0.6rem; padding-top: 1rem; border-top: 1px dashed var(--border); }
	h3 { font-size: 0.68rem; text-transform: uppercase; letter-spacing: 0.07em; color: var(--subtext0); font-weight: 650; }
	.opts { display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: 0.7rem 1.4rem; }
	.opt { display: flex; gap: 0.7rem; align-items: flex-start; }
	label { cursor: pointer; }
	.hint { display: block; font-weight: 400; font-size: 0.8rem; color: var(--subtext0); }
	.switch {
		flex: none; width: 38px; height: 22px; padding: 2px; margin-top: 0.05rem; border-radius: 999px; justify-content: flex-start;
		background: var(--surface1); border: 1px solid transparent; box-shadow: inset 0 1px 2px rgb(0 0 0 / 0.2);
	}
	.switch:hover:not(:disabled) { background: var(--surface2); border-color: transparent; }
	.switch[aria-checked='true'], .switch[aria-checked='true']:hover:not(:disabled) { background: var(--green); }
	.knob { width: 16px; height: 16px; border-radius: 50%; background: #fff; box-shadow: 0 1px 3px rgb(0 0 0 / 0.3); transition: transform 0.2s var(--ease); }
	.switch[aria-checked='true'] .knob { transform: translateX(16px); }
	.switch:disabled { opacity: 0.45; }
	.prune { display: grid; gap: 0.6rem; align-content: start; }
	.nested { margin-left: 1.2rem; }
	.danger label { color: var(--red); }
	.danger .switch[aria-checked='true'], .danger .switch[aria-checked='true']:hover:not(:disabled) { background: var(--red); }
	.note { margin: 0; color: var(--subtext1); }
</style>
