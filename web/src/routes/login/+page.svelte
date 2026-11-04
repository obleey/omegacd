<script lang="ts">
	import { goto } from '$app/navigation';
	import { api, ApiError, errMsg } from '#lib/api.ts';
	import { live } from '#lib/live.svelte.ts';

	let username = $state('admin');
	let password = $state('');
	let error = $state('');
	let busy = $state(false);

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		error = '';
		busy = true;
		try {
			const r = await api<{ mustChangePassword: boolean }>('POST', '/auth/login', { username, password });
			live.me = { username, mustChangePassword: r.mustChangePassword };
			goto(r.mustChangePassword ? '/account/password' : '/');
		} catch (err) {
			error = err instanceof ApiError && err.status === 429 ? 'Too many attempts, wait a moment.' : errMsg(err);
		} finally {
			busy = false;
		}
	}
</script>

<svelte:head><title>Log in · dockercd</title></svelte:head>

<div class="wrap">
	<form class="card" onsubmit={submit}>
		<h1><span class="logo" aria-hidden="true"></span>dockercd</h1>
		<p class="muted small">GitOps for Docker Compose</p>
		{#if error}<p class="error" role="alert">{error}</p>{/if}
		<div class="field">
			<label for="u">Username</label>
			<input id="u" bind:value={username} autocomplete="username" required />
		</div>
		<div class="field">
			<label for="p">Password</label>
			<!-- svelte-ignore a11y_autofocus -->
			<input id="p" type="password" bind:value={password} autocomplete="current-password" required autofocus />
		</div>
		<button class="primary" type="submit" disabled={busy}>{busy ? 'Logging in…' : 'Log in'}</button>
	</form>
</div>

<style>
	.wrap { min-height: 100vh; display: grid; place-items: center; padding: 1rem; }
	form { width: min(340px, 100%); padding: 1.6rem; }
	h1 { display: flex; align-items: center; gap: 0.5rem; }
	.logo { width: 20px; height: 20px; border-radius: 5px; background: var(--accent); }
	button { width: 100%; justify-content: center; margin-top: 0.3rem; }
</style>
