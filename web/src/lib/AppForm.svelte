<script lang="ts">
	import { errMsg, type Application } from './api';

	let {
		initial,
		editing = false,
		submitLabel,
		onsave,
		oncancel
	}: {
		initial?: Application;
		editing?: boolean;
		submitLabel: string;
		onsave: (a: Application) => Promise<void>;
		oncancel?: () => void;
	} = $props();

	// Form state is seeded once from `initial`; later prop changes must not clobber edits.
	const init = (() => initial)();
	let name = $state(init?.name ?? '');
	let repoURL = $state(init?.repoURL ?? '');
	let path = $state(init?.path ?? '');
	let targetRevision = $state(init?.targetRevision || 'main');
	let composeFiles = $state((init?.composeFiles ?? ['compose.yaml']).join('\n'));
	let profiles = $state((init?.profiles ?? []).join('\n'));
	let autoSync = $state(init?.autoSync ?? false);
	let selfHeal = $state(init?.selfHeal ?? false);
	let prune = $state(init?.prune ?? false);
	let adopt = $state(init?.adopt ?? false);
	let error = $state('');
	let busy = $state(false);

	const list = (s: string) =>
		s
			.split(/[,\n]/)
			.map((x) => x.trim())
			.filter(Boolean);

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		error = '';
		busy = true;
		try {
			await onsave({
				...init,
				name: name.trim(),
				repoURL: repoURL.trim(),
				path: path.trim(),
				targetRevision: targetRevision.trim() || 'main',
				composeFiles: list(composeFiles).length ? list(composeFiles) : ['compose.yaml'],
				profiles: list(profiles),
				autoSync,
				selfHeal,
				prune,
				adopt
			});
		} catch (err) {
			error = errMsg(err);
		} finally {
			busy = false;
		}
	}
</script>

<form onsubmit={submit} class="form">
	{#if error}<p class="error" role="alert">{error}</p>{/if}

	<div class="field">
		<label for="f-name">Name</label>
		<input id="f-name" bind:value={name} required disabled={editing} pattern="[a-z0-9][a-z0-9_-]*" autocomplete="off" />
		<span class="hint">Also the Compose project name. Lowercase letters, digits, - and _.</span>
	</div>
	<div class="field">
		<label for="f-repo">Repository URL</label>
		<input id="f-repo" bind:value={repoURL} required placeholder="https://github.com/org/repo.git" autocomplete="off" />
	</div>
	<div class="grid2">
		<div class="field">
			<label for="f-path">Path</label>
			<input id="f-path" bind:value={path} placeholder="." autocomplete="off" />
			<span class="hint">Directory inside the Repository.</span>
		</div>
		<div class="field">
			<label for="f-rev">Target Revision</label>
			<input id="f-rev" bind:value={targetRevision} placeholder="main" autocomplete="off" />
			<span class="hint">Branch, tag or commit.</span>
		</div>
	</div>
	<div class="grid2">
		<div class="field">
			<label for="f-files">Compose files</label>
			<textarea id="f-files" bind:value={composeFiles} rows="3"></textarea>
			<span class="hint">One per line or comma-separated, relative to Path.</span>
		</div>
		<div class="field">
			<label for="f-prof">Profiles</label>
			<textarea id="f-prof" bind:value={profiles} rows="3"></textarea>
			<span class="hint">Optional. One per line or comma-separated.</span>
		</div>
	</div>

	<fieldset>
		<legend>Sync Policy</legend>
		<div class="check">
			<input id="f-auto" type="checkbox" bind:checked={autoSync} />
			<label for="f-auto">Auto-Sync <span class="hint">Sync automatically when the Repository moves to a new Revision.</span></label>
		</div>
		<div class="check">
			<input id="f-heal" type="checkbox" bind:checked={selfHeal} />
			<label for="f-heal">Self-Heal <span class="hint">Sync automatically when the Live State drifts without a new Revision.</span></label>
		</div>
		<div class="check">
			<input id="f-prune" type="checkbox" bind:checked={prune} />
			<label for="f-prune">Prune <span class="hint">Remove containers and networks no longer in the Desired State. Volumes are never removed.</span></label>
		</div>
		{#if !editing}
			<div class="check">
				<input id="f-adopt" type="checkbox" bind:checked={adopt} />
				<label for="f-adopt">Adopt <span class="hint">Take over an existing, unmanaged Compose project with the same name.</span></label>
			</div>
		{/if}
	</fieldset>

	<div class="row">
		<button class="primary" type="submit" disabled={busy}>{busy ? 'Saving…' : submitLabel}</button>
		{#if oncancel}<button type="button" onclick={oncancel}>Cancel</button>{/if}
	</div>
</form>

<style>
	.form { max-width: 720px; }
	.grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: 0 1rem; }
	fieldset { border: 1px solid var(--surface0); border-radius: 8px; padding: 0.8rem 1rem 0.2rem; margin: 0 0 1.2rem; }
	legend { padding: 0 0.4rem; font-weight: 600; }
	.check .hint { display: block; }
	@media (max-width: 640px) { .grid2 { grid-template-columns: 1fr; } }
</style>
