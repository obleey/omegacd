// The viewer's display preferences, kept in this browser (Q169).
import { type DateFormat, dateFormats, formatDate } from './datetime.ts';

const KEY = 'omegacd.dateFormat';

function read(): DateFormat {
	try {
		const v = localStorage.getItem(KEY) as DateFormat | null;
		return v && v in dateFormats ? v : 'browser';
	} catch {
		return 'browser'; // storage blocked: the browser's format
	}
}

export const display = $state({ dateFormat: read() });

export function setDateFormat(f: DateFormat) {
	display.dateFormat = f;
	try {
		if (f === 'browser') localStorage.removeItem(KEY);
		else localStorage.setItem(KEY, f);
	} catch {
		// kept for this page only
	}
}

/** A timestamp in the viewer's chosen format. */
export const when = (ts: string, what: 'datetime' | 'time' = 'datetime') => formatDate(ts, display.dateFormat, what);
