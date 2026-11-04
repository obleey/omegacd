<script lang="ts">
	import { goto } from '$app/navigation';
	import { api, errMsg } from '#lib/api.ts';
	import { live } from '#lib/live.svelte.ts';

	let currentPassword = $state('');
	let newPassword = $state('');
	let confirm = $state('');
	let error = $state('');
	let busy = $state(false);

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		error = '';
		if (newPassword !== confirm) {
			error = 'The new passwords do not match.';
			return;
		}
		busy = true;
		try {
			await api('POST', '/auth/password', { currentPassword, newPassword });
			if (live.me) live.me.mustChangePassword = false;
			goto('/');
		} catch (err) {
			error = errMsg(err);
		} finally {
			busy = false;
		}
	}
</script>

<svelte:head><title>Change password · dockercd</title></svelte:head>

<h1>Change password</h1>
{#if live.me?.mustChangePassword}
	<p class="banner" role="status">You must set a new password before continuing.</p>
{/if}
<form class="card" onsubmit={submit}>
	{#if error}<p class="error" role="alert">{error}</p>{/if}
	<div class="field">
		<label for="cur">Current password</label>
		<input id="cur" type="password" bind:value={currentPassword} autocomplete="current-password" required />
	</div>
	<div class="field">
		<label for="new">New password</label>
		<input id="new" type="password" bind:value={newPassword} autocomplete="new-password" required minlength="8" />
	</div>
	<div class="field">
		<label for="conf">Confirm new password</label>
		<input id="conf" type="password" bind:value={confirm} autocomplete="new-password" required />
	</div>
	<button class="primary" type="submit" disabled={busy}>{busy ? 'Saving…' : 'Change password'}</button>
</form>

<style>
	h1 { margin-bottom: 1rem; }
	.banner { margin-bottom: 1rem; max-width: 420px; }
	form { max-width: 420px; }
</style>
