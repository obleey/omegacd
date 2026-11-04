<script lang="ts">
	import { goto } from '$app/navigation';
	import { api, enc, type AppView } from '#lib/api.ts';
	import { putApp } from '#lib/live.svelte.ts';
	import AppForm from '#lib/AppForm.svelte';
</script>

<svelte:head><title>New Application · dockercd</title></svelte:head>

<nav class="crumbs small" aria-label="Breadcrumb"><a href="/">Applications</a> / <span aria-current="page">New</span></nav>
<h1>New Application</h1>
<p class="muted small sub">Deploy a Compose project, or a set of Applications, from a path in a Git Repository.</p>
<div class="card">
	<AppForm
		submitLabel="Create"
		oncancel={() => goto('/')}
		onsave={async (a) => {
			const v = await api<AppView>('POST', '/apps', a);
			putApp(v);
			goto(`/apps/${enc(v.name)}`);
		}}
	/>
</div>

<style>
	.crumbs { color: var(--subtext0); margin-bottom: 0.8rem; }
	.crumbs a { color: var(--subtext0); }
	.sub { margin: 0.15rem 0 1.1rem; }
	.card { padding: 1.4rem 1.5rem; }
</style>
