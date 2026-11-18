// A container's uptime counted live from the start time its Agent reports (Q179), instead of the engine's status line
// frozen at the report ("Up 3 seconds").
import type { Container } from './api.ts';

/** 45 s, 3 min, 2 h 14 min, 3 d 4 h. */
export function duration(ms: number): string {
	const s = Math.max(0, Math.floor(ms / 1000));
	if (s < 60) return `${s} s`;
	const m = Math.floor(s / 60);
	if (m < 60) return `${m} min`;
	const h = Math.floor(m / 60);
	if (h < 24) return m % 60 ? `${h} h ${m % 60} min` : `${h} h`;
	const d = Math.floor(h / 24);
	return h % 24 ? `${d} d ${h % 24} h` : `${d} d`;
}

/** "Up 2 h 14 min", "Exited (1) 5 min ago"; the engine's status line when the times aren't known (an older Agent). */
export function uptime(c: Container, now: number): string {
	if (c.state === 'running' && c.startedAt) return `Up ${duration(now - Date.parse(c.startedAt))}`;
	if ((c.state === 'exited' || c.state === 'dead') && c.finishedAt)
		return `Exited (${c.exitCode}) ${duration(now - Date.parse(c.finishedAt))} ago`;
	return c.status;
}
