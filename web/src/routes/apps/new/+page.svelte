<script lang="ts">
	import { goto } from '$app/navigation';
	import { api, enc, type AppView } from '#lib/api.ts';
	import { putApp } from '#lib/live.svelte.ts';
	import AppForm from '#lib/AppForm.svelte';
</script>

<svelte:head><title>New Application · dockercd</title></svelte:head>

<p class="small"><a href="/">← Applications</a></p>
<h1>New Application</h1>
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
	h1 { margin: 0.3rem 0 1rem; }
</style>
