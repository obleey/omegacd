import { sveltekit } from '@sveltejs/kit/vite';
import adapter from '@sveltejs/adapter-static';
import { defineConfig } from 'vite';

export default defineConfig({
	plugins: [sveltekit({ adapter: adapter({ fallback: 'index.html' }) })],
	// `npm run dev` talks to a dockercd running locally
	server: { proxy: { '/api': 'http://localhost:8080' } }
});
