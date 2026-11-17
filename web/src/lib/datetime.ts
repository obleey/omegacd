// Dates and times as the viewer chose (Q168): the browser's own format, European 24-hour or US 12-hour.

export type DateFormat = 'browser' | 'eu' | 'us';
export const dateFormats: Record<DateFormat, string> = { browser: 'Browser default', eu: 'European, 24-hour', us: 'US, 12-hour' };

const pad = (n: number) => String(n).padStart(2, '0');

/** A date and time, or with what 'time' only the time, in the viewer's time zone. */
export function formatDate(ts: string | number | Date, format: DateFormat, what: 'datetime' | 'time' = 'datetime'): string {
	const d = new Date(ts);
	if (format === 'browser') return what === 'time' ? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : d.toLocaleString();
	let time: string, date: string;
	if (format === 'eu') {
		time = `${pad(d.getHours())}:${pad(d.getMinutes())}`;
		date = `${pad(d.getDate())}.${pad(d.getMonth() + 1)}.${d.getFullYear()}`;
	} else {
		time = `${d.getHours() % 12 || 12}:${pad(d.getMinutes())} ${d.getHours() < 12 ? 'AM' : 'PM'}`;
		date = `${d.getMonth() + 1}/${d.getDate()}/${d.getFullYear()}`;
	}
	return what === 'time' ? time : `${date} ${time}`;
}
