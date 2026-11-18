// Sizes as people read them, in powers of 1024: what Settings shows of the Source Server's caches (Q202).

/** 0 B, 512 B, 2.0 KiB, 1.5 MiB, 3.2 GiB. */
export function bytes(n: number): string {
	const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
	let i = 0;
	while (n >= 1024 && i < units.length - 1) {
		n /= 1024;
		i++;
	}
	return i ? `${n.toFixed(1)} ${units[i]}` : `${n} B`;
}
