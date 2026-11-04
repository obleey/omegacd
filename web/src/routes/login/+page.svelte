<script lang="ts">
	import { goto } from '$app/navigation';
	import { api, ApiError, errMsg } from '#lib/api.ts';
	import { live } from '#lib/live.svelte.ts';
	import Logo from '#lib/Logo.svelte';
	import Icon from '#lib/Icon.svelte';
	import Version from '#lib/Version.svelte';

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

<svelte:head><title>Log in · OmegaCD</title></svelte:head>

<div class="wrap">
	<div class="glow g1" aria-hidden="true"></div>
	<div class="glow g2" aria-hidden="true"></div>
	<form class="card" onsubmit={submit}>
		<div class="brand">
			<Logo size={48} />
			<h1>OmegaCD</h1>
			<Version />
			<p class="muted">GitOps continuous delivery for Docker Compose</p>
		</div>
		{#if error}<p class="error" role="alert"><Icon name="alert" size={16} />{error}</p>{/if}
		<div class="field">
			<label for="u">Username</label>
			<input id="u" bind:value={username} autocomplete="username" required />
		</div>
		<div class="field">
			<label for="p">Password</label>
			<!-- svelte-ignore a11y_autofocus -->
			<input id="p" type="password" bind:value={password} autocomplete="current-password" required autofocus />
		</div>
		<button class="primary" type="submit" disabled={busy}>
			{#if busy}<Icon name="notch" size={15} spin />Logging in…{:else}Log in{/if}
		</button>
	</form>
	<p class="foot muted small">Keeps Compose projects in line with Git.</p>
</div>

<style>
	.wrap { position: relative; min-height: 100vh; display: grid; place-items: center; align-content: center; gap: 1.2rem; padding: 1rem; overflow: hidden; background: radial-gradient(ellipse at top, color-mix(in srgb, var(--mauve) 10%, var(--bg)), var(--bg) 60%); }
	.glow { position: absolute; width: 520px; height: 520px; border-radius: 50%; filter: blur(90px); opacity: 0.28; pointer-events: none; animation: drift 14s ease-in-out infinite alternate; }
	.g1 { background: var(--mauve); top: -180px; left: calc(50% - 520px); }
	.g2 { background: var(--blue); bottom: -220px; left: calc(50% + 20px); animation-delay: -7s; }
	@keyframes drift { to { transform: translate(60px, 40px) scale(1.1); } }
	form { position: relative; width: min(380px, 100%); padding: 2rem 1.8rem 1.6rem; box-shadow: var(--shadow-lg); background: color-mix(in srgb, var(--card) 88%, transparent); backdrop-filter: blur(12px); animation: rise 0.5s var(--ease) both; }
	@keyframes rise { from { opacity: 0; transform: translateY(12px) scale(0.98); } }
	.brand { display: grid; justify-items: center; text-align: center; gap: 0.35rem; margin-bottom: 1.4rem; }
	.brand h1 { font-size: 1.6rem; margin-top: 0.4rem; }
	.brand p { margin: 0; font-size: 0.88rem; }
	.error { margin: 0 0 1rem; }
	button { width: 100%; margin-top: 0.4rem; padding: 0.55rem; }
	.foot { position: relative; margin: 0; }
</style>
