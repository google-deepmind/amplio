/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
	buildSpec,
	clockOffset,
	defaultEnds,
	describeRule,
	endsAt,
	describeSpec,
	formatAhead,
	editFrom,
	fieldsForEdit,
	loopPatch,
	formatNext,
	formatShort,
	formatWhen,
	parseSpec,
	splitTick,
	tickLabel,
	validate,
	type EditFrom,
	type LoopFields
} from './loopSchedule.ts';

const LA = 'America/Los_Angeles';
const NOW = Date.UTC(2026, 10, 10, 23, 30, 15, 500); // Tue Nov 10 15:30:15.5 PST
const MIN = 60_000;
const fields = (f: Partial<LoopFields>): LoopFields => ({
	every: 30,
	unit: 'minutes',
	sendNow: true,
	ends: { kind: 'after', n: 7, unit: 'days' },
	...f
});

test('buildSpec: send now, every 30 minutes, ends after 7 days', () => {
	assert.equal(buildSpec(fields({}), NOW), 'DTSTART:20261110T233015Z\nRRULE:FREQ=MINUTELY;INTERVAL=30;UNTIL=20261117T233015Z');
});

test('buildSpec: without send now, the first tick is one interval later', () => {
	assert.equal(
		buildSpec(fields({ sendNow: false, ends: { kind: 'never' } }), NOW),
		'DTSTART:20261111T000015Z\nRRULE:FREQ=MINUTELY;INTERVAL=30'
	);
});

test('buildSpec: the frequency is the largest unit that divides the interval', () => {
	const rule = (f: Partial<LoopFields>) => buildSpec(fields({ ends: { kind: 'never' }, ...f }), NOW).split('\n')[1];
	assert.equal(rule({ every: 2, unit: 'hours' }), 'RRULE:FREQ=HOURLY;INTERVAL=2');
	assert.equal(rule({ every: 90 }), 'RRULE:FREQ=MINUTELY;INTERVAL=90');
	assert.equal(rule({ every: 120 }), 'RRULE:FREQ=HOURLY;INTERVAL=2');
	assert.equal(rule({ every: 24, unit: 'hours' }), 'RRULE:FREQ=DAILY;INTERVAL=1');
	assert.equal(rule({ every: 3, unit: 'days' }), 'RRULE:FREQ=DAILY;INTERVAL=3');
});

test('buildSpec: ends after N ticks', () => {
	assert.equal(
		buildSpec(fields({ every: 1, unit: 'hours', ends: { kind: 'times', n: 5 } }), NOW),
		'DTSTART:20261110T233015Z\nRRULE:FREQ=HOURLY;INTERVAL=1;COUNT=5'
	);
});

// A loop every hour that last ticked `ago` before NOW, next due an hour after.
const ticked = (ago: number): EditFrom => ({ seconds: 3600, next: NOW - ago + 60 * MIN, last: NOW - ago, created: NOW - 600 * MIN });

test('buildSpec: a new interval on a loop that has ticked starts one interval after its last tick, or now', () => {
	const f = fields({ ends: { kind: 'never' } });
	assert.equal(buildSpec(f, NOW, ticked(10 * MIN)).split('\n')[0], 'DTSTART:20261110T235015Z');
	assert.equal(buildSpec(f, NOW, ticked(120 * MIN)).split('\n')[0], 'DTSTART:20261110T233015Z');
	// send now doesn't apply to an edit
	assert.equal(buildSpec({ ...f, sendNow: false }, NOW, ticked(120 * MIN)).split('\n')[0], 'DTSTART:20261110T233015Z');
});

test('buildSpec: an edit that keeps the interval keeps the rule\'s grid', () => {
	const f = fields({ every: 1, unit: 'hours', ends: { kind: 'times', n: 9 } });
	assert.equal(buildSpec(f, NOW, ticked(10 * MIN)), 'DTSTART:20261111T002015Z\nRRULE:FREQ=HOURLY;INTERVAL=1;COUNT=9');
	// the last tick was held 40 s past its grid point: the grid, not the tick, sets the next
	const held = { ...ticked(10 * MIN), last: NOW - 10 * MIN + 40_000 };
	assert.equal(buildSpec(f, NOW, held).split('\n')[0], 'DTSTART:20261111T002015Z');
	// its next occurrence is due (held): now
	assert.equal(buildSpec(f, NOW, ticked(61 * MIN)).split('\n')[0], 'DTSTART:20261110T233015Z');
});

test('buildSpec: an edit of a loop that hasn\'t ticked keeps its start, whatever the interval', () => {
	const start = Date.UTC(2026, 11, 1, 9);
	const from: EditFrom = { seconds: 86400, next: start, last: null, created: NOW - 168 * 60 * MIN };
	for (const f of [fields({ every: 1, unit: 'days', ends: { kind: 'times', n: 60 } }), fields({ every: 2, unit: 'days' })]) {
		assert.equal(buildSpec(f, NOW, from).split('\n')[0], 'DTSTART:20261201T090000Z');
	}
	// one whose start has passed (a tick held) starts now
	assert.equal(buildSpec(fields({}), NOW, { ...from, next: NOW - 5_000 }).split('\n')[0], 'DTSTART:20261110T233015Z');
	// none left to keep: one new interval after it was set, or now
	assert.equal(buildSpec(fields({}), NOW, { ...from, next: null, created: NOW - MIN }).split('\n')[0], 'DTSTART:20261110T235915Z');
});

test('buildSpec: an edit keeps an existing end date', () => {
	const at = Date.UTC(2026, 10, 17, 23, 30, 0);
	assert.equal(
		buildSpec(fields({ ends: { kind: 'until', at } }), NOW, { seconds: 3600, next: NOW + 59 * MIN, last: NOW - MIN, created: NOW - 9 * MIN }),
		'DTSTART:20261110T235915Z\nRRULE:FREQ=MINUTELY;INTERVAL=30;UNTIL=20261117T233000Z'
	);
});

test('validate', () => {
	const v = (f: Partial<LoopFields>, min = 60, edit?: EditFrom) => validate(fields(f), min, NOW, edit);
	assert.equal(v({}), null);
	assert.match(v({ every: null }) ?? '', /how often/);
	assert.match(v({ every: 0 }) ?? '', /whole number/);
	assert.match(v({ every: 1.5, unit: 'hours' }) ?? '', /whole number/);
	assert.equal(v({ every: 1 }), null);
	assert.match(v({ every: 1 }, 120) ?? '', /at most every 2 minutes/);
	assert.match(v({ ends: { kind: 'times', n: null } }) ?? '', /how many/);
	assert.match(v({ ends: { kind: 'times', n: 0 } }) ?? '', /how many/);
	assert.match(v({ ends: { kind: 'after', n: null, unit: 'hours' } }) ?? '', /how long/);
	assert.match(v({ every: 1, unit: 'days', sendNow: false, ends: { kind: 'after', n: 1, unit: 'hours' } }) ?? '', /end before/);
	assert.equal(v({ every: 1, unit: 'days', ends: { kind: 'after', n: 1, unit: 'hours' } }), null); // send now: tick 1 is now
	assert.match(v({ ends: { kind: 'until', at: NOW + 5 * MIN } }, 60, ticked(0)) ?? '', /end before/);
});

test('validate: the server\'s INTERVAL limit and sane ends, never a rule that throws', () => {
	const v = (f: Partial<LoopFields>) => validate(fields(f), 60, NOW);
	assert.equal(v({ every: 100000 }), null);
	assert.match(v({ every: 100001 }) ?? '', /up to 100000 minutes/);
	assert.equal(v({ every: 6000000 }), null); // 100000 hours, written HOURLY
	assert.match(v({ every: 6000060 }) ?? '', /up to 100000 hours/); // 100001 hours
	assert.match(v({ every: 6000001 }) ?? '', /up to 100000 minutes/);
	assert.match(v({ every: 100001, unit: 'days' }) ?? '', /up to 100000 days/);
	assert.match(v({ every: 1e21 }) ?? '', /whole number/);
	assert.equal(v({ ends: { kind: 'after', n: 3660, unit: 'days' } }), null);
	assert.match(v({ ends: { kind: 'after', n: 3661, unit: 'days' } }) ?? '', /10 years/);
	assert.match(v({ ends: { kind: 'after', n: 1e8, unit: 'days' } }) ?? '', /10 years/);
	assert.match(v({ ends: { kind: 'after', n: 1e21, unit: 'days' } }) ?? '', /how long/);
	assert.match(v({ ends: { kind: 'times', n: 1000001 } }) ?? '', /up to 1000000 times/);
	// no end date to show for a refused duration (it isn't a valid date)
	assert.equal(endsAt(fields({ ends: { kind: 'after', n: 1e8, unit: 'days' } }), NOW), null);
	assert.equal(endsAt(fields({ ends: { kind: 'after', n: 3660, unit: 'days' } }), NOW), NOW - 500 + 3660 * 86400_000);
	// whatever validate accepts builds a rule without throwing
	assert.doesNotThrow(() => buildSpec(fields({ ends: { kind: 'after', n: 3660, unit: 'days' } }), NOW));
});

test('parseSpec reads the form\'s rules and nothing else', () => {
	const spec = buildSpec(fields({ ends: { kind: 'times', n: 4 } }), NOW);
	assert.deepEqual(parseSpec(spec), { start: Date.UTC(2026, 10, 10, 23, 30, 15), seconds: 1800, count: 4 });
	assert.deepEqual(parseSpec('DTSTART:20261110T233015Z RRULE:FREQ=HOURLY'), { start: Date.UTC(2026, 10, 10, 23, 30, 15), seconds: 3600 });
	assert.deepEqual(parseSpec('DTSTART:20261110T233015Z\nRRULE:FREQ=DAILY;INTERVAL=2;UNTIL=20261117T000000Z'), {
		start: Date.UTC(2026, 10, 10, 23, 30, 15),
		seconds: 172800,
		until: Date.UTC(2026, 10, 17)
	});
	for (const other of [
		'DTSTART:20261110T090000\nRRULE:FREQ=DAILY', // floating
		'DTSTART;TZID=Europe/Zurich:20261110T090000\nRRULE:FREQ=DAILY',
		'DTSTART:20261110T090000Z\nRRULE:FREQ=WEEKLY;BYDAY=MO,WE,FR',
		'DTSTART:20261110T090000Z\nRRULE:FREQ=HOURLY;BYHOUR=9',
		'DTSTART:20261110T090000Z\nRRULE:FREQ=SECONDLY;INTERVAL=90',
		'DTSTART:20261110T090000Z\nRRULE:FREQ=HOURLY\nRDATE:20261110T093000Z',
		'DTSTART:20261110T090000Z\nRRULE:FREQ=HOURLY;COUNT=2;UNTIL=20261111T000000Z',
		'DTSTART:20261110T090000Z\nRRULE:FREQ=HOURLY;INTERVAL=0',
		'RRULE:FREQ=HOURLY',
		'nonsense'
	]) {
		assert.equal(parseSpec(other), null, other);
	}
});

test('describeRule: a calendar\'s one-line summary, times in the server\'s zone', () => {
	const until = Date.UTC(2026, 10, 17, 23, 30);
	assert.equal(describeRule({ start: NOW, seconds: 1800, until }, LA, NOW), 'Every 30 minutes, until Tue Nov 17 15:30');
	assert.equal(describeRule({ start: NOW, seconds: 1800, until }, 'Europe/Zurich', NOW), 'Every 30 minutes, until Wed Nov 18 00:30');
	assert.equal(describeRule({ start: NOW, seconds: 3600, count: 5 }, LA, NOW), 'Every hour, 5 times');
	assert.equal(describeRule({ start: NOW, seconds: 60, count: 1 }, LA, NOW), 'Every minute, once');
	assert.equal(describeRule({ start: NOW, seconds: 172800 }, LA, NOW), 'Every 2 days');
	assert.equal(describeRule({ start: NOW, seconds: 5400 }, LA, NOW), 'Every 90 minutes');
	assert.equal(
		describeRule({ start: NOW, seconds: 86400, until: Date.UTC(2027, 0, 3, 17) }, LA, NOW),
		'Every day, until Sun Jan 3 2027 09:00'
	);
});

test('describeSpec falls back to the server\'s summary', () => {
	assert.equal(describeSpec('DTSTART:20261110T233015Z\nRRULE:FREQ=HOURLY;COUNT=5', 'every 1h, 5 times', LA, NOW), 'Every hour, 5 times');
	assert.equal(
		describeSpec('DTSTART:20261110T090000\nRRULE:FREQ=WEEKLY;BYDAY=MO', 'RRULE:FREQ=WEEKLY;BYDAY=MO', LA, NOW),
		'RRULE:FREQ=WEEKLY;BYDAY=MO'
	);
});

test('fieldsForEdit keeps the end: a date, or the ticks left', () => {
	const start = Date.UTC(2026, 10, 10, 23, 0);
	const spec = (rule: string) => `DTSTART:20261110T230000Z\nRRULE:${rule}`;
	assert.deepEqual(fieldsForEdit(spec('FREQ=MINUTELY;INTERVAL=30;COUNT=5'), start + 2 * 30 * MIN), {
		every: 30,
		unit: 'minutes',
		sendNow: false,
		ends: { kind: 'times', n: 3 }
	});
	assert.deepEqual(fieldsForEdit(spec('FREQ=MINUTELY;INTERVAL=120;UNTIL=20261117T230000Z'), null), {
		every: 2,
		unit: 'hours',
		sendNow: false,
		ends: { kind: 'until', at: Date.UTC(2026, 10, 17, 23) }
	});
	// a tick held 35 s past its grid point still leaves the same count
	assert.deepEqual(fieldsForEdit(spec('FREQ=MINUTELY;INTERVAL=1;COUNT=5'), start + 2 * MIN + 35_000)?.ends, { kind: 'times', n: 3 });
	assert.deepEqual(fieldsForEdit(spec('FREQ=DAILY'), start + 86400_000)?.ends, { kind: 'never' });
	assert.equal(fieldsForEdit(spec('FREQ=WEEKLY;BYDAY=MO'), null), null);
});

test('editFrom: the next occurrence is the grid point of the next tick', () => {
	const start = Date.UTC(2026, 10, 10, 23, 0);
	const loop = {
		schedule: { spec: 'DTSTART:20261110T230000Z\nRRULE:FREQ=MINUTELY;INTERVAL=30;COUNT=5' },
		created_at: '2026-11-10T22:59:58Z',
		last_tick: '2026-11-10T23:31:10Z',
		next_tick: '2026-11-11T00:01:10Z' // held 70 s past its grid point
	};
	assert.deepEqual(editFrom(loop), {
		seconds: 1800,
		next: start + 2 * 30 * MIN,
		last: Date.parse('2026-11-10T23:31:10Z'),
		created: Date.parse('2026-11-10T22:59:58Z')
	});
	assert.deepEqual(editFrom({ ...loop, last_tick: undefined, next_tick: '2026-11-10T23:00:00Z' }), {
		seconds: 1800,
		next: start,
		last: null,
		created: Date.parse('2026-11-10T22:59:58Z')
	});
	assert.equal(editFrom({ ...loop, next_tick: undefined })?.next, null);
	assert.equal(editFrom({ ...loop, schedule: { spec: 'DTSTART:20261110T090000\nRRULE:FREQ=WEEKLY;BYDAY=MO' } }), undefined);
});

test('an edit of only the COUNT of a loop that hasn\'t ticked keeps its start (a rule set from the CLI)', () => {
	const spec = 'DTSTART:20261201T090000Z\nRRULE:FREQ=DAILY;INTERVAL=1;COUNT=30';
	for (const createdAgo of [60 * MIN, 168 * 60 * MIN]) {
		const loop = { schedule: { spec }, created_at: new Date(NOW - createdAgo).toISOString(), next_tick: '2026-12-01T09:00:00Z' };
		const opened = fieldsForEdit(spec, Date.parse(loop.next_tick))!;
		assert.deepEqual(opened, { every: 1, unit: 'days', sendNow: false, ends: { kind: 'times', n: 30 } });
		assert.equal(
			buildSpec({ ...opened, ends: { kind: 'times', n: 60 } }, NOW, editFrom(loop)),
			'DTSTART:20261201T090000Z\nRRULE:FREQ=DAILY;INTERVAL=1;COUNT=60'
		);
	}
});

test('defaultEnds reads [loop] expiry', () => {
	assert.deepEqual(defaultEnds('168h'), { kind: 'after', n: 7, unit: 'days' });
	assert.deepEqual(defaultEnds('36h'), { kind: 'after', n: 36, unit: 'hours' });
	assert.deepEqual(defaultEnds('1h30m'), { kind: 'after', n: 90, unit: 'minutes' });
	assert.deepEqual(defaultEnds('never'), { kind: 'never' });
	assert.deepEqual(defaultEnds(''), { kind: 'after', n: 7, unit: 'days' });
	assert.deepEqual(defaultEnds(undefined), { kind: 'after', n: 7, unit: 'days' });
	assert.deepEqual(defaultEnds('soon'), { kind: 'after', n: 7, unit: 'days' });
});

test('formatWhen and formatShort', () => {
	assert.equal(formatWhen(Date.UTC(2026, 10, 17, 23, 30), LA, NOW, { zone: true }), 'Tue Nov 17 15:30 PST');
	assert.equal(formatShort(Date.UTC(2026, 10, 11, 1, 0), LA, NOW), '17:00');
	assert.equal(formatShort(Date.UTC(2026, 10, 17, 23, 30), LA, NOW), 'Nov 17');
	assert.equal(formatShort(Date.UTC(2027, 0, 3, 17), LA, NOW), 'Jan 3 2027');
});

test('formatNext: the time alone today, else with the day', () => {
	assert.equal(formatNext(Date.UTC(2026, 10, 11, 1, 0), LA, NOW), '17:00');
	assert.equal(formatNext(Date.UTC(2026, 10, 12, 17, 0), LA, NOW), 'Thu Nov 12 09:00');
});

test('formatAhead: the time today, the weekday within the next 6 days, else the date', () => {
	const at = (d: number, h: number, m = 0) => Date.UTC(2026, 10, d, h + 8, m); // PST
	assert.equal(formatAhead(at(10, 23, 59), LA, NOW), '23:59'); // Tue, today
	assert.equal(formatAhead(at(10, 9), LA, NOW), '09:00'); // overdue today
	assert.equal(formatAhead(at(11, 0, 5), LA, NOW), 'Wed 00:05');
	assert.equal(formatAhead(at(16, 12, 55), LA, NOW), 'Mon 12:55'); // 6 days on
	assert.equal(formatAhead(at(17, 12, 55), LA, NOW), 'Nov 17'); // a Tuesday again
	assert.equal(formatAhead(at(9, 12), LA, NOW), 'Nov 9'); // overdue since yesterday
	assert.equal(formatAhead(Date.UTC(2027, 0, 3, 17), LA, NOW), 'Jan 3 2027');
	// The zone's calendar, not UTC's: 23:30 PST is already tomorrow in UTC.
	assert.equal(formatAhead(at(10, 23, 30), LA, NOW), '23:30');
	assert.equal(formatAhead(at(11, 23, 30), 'UTC', NOW), 'Thu 07:30');
});

test('splitTick takes a tick\'s header apart', () => {
	assert.deepEqual(splitTick('[loop 2 · every 1m, 3 times · tick 3] check · the eval', '2', 3), {
		summary: 'every 1m, 3 times',
		prompt: 'check · the eval'
	});
	assert.deepEqual(splitTick('[loop 1 · RRULE:FREQ=WEEKLY;BYDAY=MO · tick 1] a\nb', '1', 1), {
		summary: 'RRULE:FREQ=WEEKLY;BYDAY=MO',
		prompt: 'a\nb'
	});
	assert.deepEqual(splitTick('[loop 1 · every 1h, 2 times · tick 2 · verification v3] status check', '1', 2), {
		summary: 'every 1h, 2 times',
		prompt: 'status check'
	});
	assert.equal(splitTick('[loop 1 · every 1m · tick 2 · verification v] x', '1', 2), null);
	assert.equal(splitTick('[loop 1 · every 1m · tick 2] x', '1', 3), null);
	assert.equal(splitTick('[loop 12 · every 1m · tick 2] x', '1', 2), null);
	assert.equal(splitTick('check the eval', '1', 1), null);
});

test('tickLabel names the loop, the tick and the verification version it started', () => {
	assert.equal(tickLabel({ id: '1', n: 2 }), 'loop 1 · tick 2');
	assert.equal(tickLabel({ id: '1', n: 2, version: 3 }), 'loop 1 · tick 2 · verification v3');
});

test('clockOffset: the server\'s clock against the middle of the round trip', () => {
	assert.equal(clockOffset('2026-11-10T15:40:00-08:00', Date.UTC(2026, 10, 10, 23, 30), Date.UTC(2026, 10, 10, 23, 30, 2)), 599_000);
	assert.equal(clockOffset('2026-11-10T23:29:00.5Z', Date.UTC(2026, 10, 10, 23, 30), Date.UTC(2026, 10, 10, 23, 30, 1)), -60_000);
	assert.equal(clockOffset(undefined, 1, 2), 0);
	assert.equal(clockOffset('nonsense', 1, 2), 0);
});

test('loopPatch sends only what changed, a schedule rebuilt from the last tick', () => {
	const spec = 'DTSTART:20261110T220000Z\nRRULE:FREQ=MINUTELY;INTERVAL=20;COUNT=10';
	const loop = {
		id: '4',
		schedule: { spec },
		created_at: '2026-11-10T14:00:00-08:00',
		last_tick: '2026-11-10T15:20:00-08:00',
		next_tick: '2026-11-10T15:40:00-08:00',
		prompt: 'check',
		ticks: 5
	};
	const before = fieldsForEdit(spec, Date.UTC(2026, 10, 10, 23, 40));
	assert.deepEqual(before, { every: 20, unit: 'minutes', sendNow: false, ends: { kind: 'times', n: 5 } });
	if (!before) return;
	const opened = { fields: before, ticks: 5, prompt: 'check' };
	assert.deepEqual(loopPatch(loop, opened, { ...before }, 'check', NOW), {});
	assert.deepEqual(loopPatch(loop, opened, before, ' check again\n', NOW), { prompt: 'check again' });
	assert.deepEqual(loopPatch(loop, opened, before, 'check \n', NOW), {}); // only whitespace changed
	// last tick 15:20 PST + 1 h = 16:20 PST, after now (15:30)
	assert.deepEqual(loopPatch(loop, opened, { ...before, every: 1, unit: 'hours' }, 'check', NOW), {
		schedule: { kind: 'rrule', spec: 'DTSTART:20261111T002000Z\nRRULE:FREQ=HOURLY;INTERVAL=1;COUNT=5' }
	});
	// only the end: the next tick stays 15:40 PST, on the rule's grid
	assert.deepEqual(loopPatch(loop, opened, { ...before, ends: { kind: 'times', n: 8 } }, 'check', NOW), {
		schedule: { kind: 'rrule', spec: 'DTSTART:20261110T234000Z\nRRULE:FREQ=MINUTELY;INTERVAL=20;COUNT=8' }
	});
	// a rule the form can't show: only the prompt can change
	assert.deepEqual(loopPatch(loop, { fields: null, ticks: 5, prompt: 'check' }, null, 'x', NOW), { prompt: 'x' });
	// one changed meanwhile (the CLI) to a rule the form can't write: refused
	const weekly = { ...loop, schedule: { spec: 'DTSTART:20261110T090000\nRRULE:FREQ=WEEKLY;BYDAY=MO' } };
	assert.throws(() => loopPatch(weekly, opened, { ...before, every: 2 }, 'check', NOW), /edit it again/);
});

test('loopPatch: editing only the COUNT of a loop that hasn\'t ticked keeps its start (a rule set from the CLI)', () => {
	const spec = 'DTSTART:20261201T090000Z\nRRULE:FREQ=DAILY;INTERVAL=1;COUNT=30';
	for (const createdAgo of [60 * MIN, 168 * 60 * MIN]) {
		const loop = { id: '1', schedule: { spec }, created_at: new Date(NOW - createdAgo).toISOString(), next_tick: '2026-12-01T09:00:00Z', prompt: 'report', ticks: 0 };
		const opened = { fields: fieldsForEdit(spec, Date.parse(loop.next_tick)), ticks: 0, prompt: 'report' };
		const after: LoopFields = { ...opened.fields!, ends: { kind: 'times', n: 60 } };
		assert.deepEqual(loopPatch(loop, opened, after, 'report', NOW), {
			schedule: { kind: 'rrule', spec: 'DTSTART:20261201T090000Z\nRRULE:FREQ=DAILY;INTERVAL=1;COUNT=60' }
		});
	}
});

test('loopPatch: a prompt changed elsewhere while the form was open is not sent back', () => {
	const spec = 'DTSTART:20261112T100000Z\nRRULE:FREQ=HOURLY;INTERVAL=1;COUNT=5';
	const atOpen = { id: '1', schedule: { spec }, created_at: '2026-11-12T09:00:00Z', next_tick: '2026-11-12T10:00:00Z', prompt: 'run the tests', ticks: 0 };
	const opened = { fields: fieldsForEdit(spec, Date.parse(atOpen.next_tick)), ticks: 0, prompt: atOpen.prompt };
	const now = { ...atOpen, prompt: 'run the tests and the linter' }; // another tab, or the CLI
	const t = Date.parse('2026-11-12T09:30:00Z');
	assert.deepEqual(loopPatch(now, opened, opened.fields, atOpen.prompt, t), {});
	assert.deepEqual(Object.keys(loopPatch(now, opened, { ...opened.fields!, every: 2 }, atOpen.prompt, t)), ['schedule']);
	// the operator's own change to it is sent
	assert.deepEqual(loopPatch(now, opened, opened.fields, 'run the linter', t), { prompt: 'run the linter' });
});

test('loopPatch: a tick during the edit changes nothing the operator didn\'t', () => {
	const before = { every: 1, unit: 'minutes', sendNow: false, ends: { kind: 'times', n: 3 } } as LoopFields;
	const opened = { fields: before, ticks: 2, prompt: 'p' };
	const ticked = {
		id: '4',
		schedule: { spec: 'DTSTART:20261110T232800Z\nRRULE:FREQ=MINUTELY;INTERVAL=1;COUNT=5' },
		created_at: '2026-11-10T15:00:00-08:00',
		last_tick: '2026-11-10T15:30:00-08:00',
		next_tick: '2026-11-10T15:31:00-08:00',
		prompt: 'p',
		ticks: 3
	};
	// only the prompt changed: no schedule, so no extra tick
	assert.deepEqual(loopPatch(ticked, opened, before, 'p2', NOW), { prompt: 'p2' });
	// a new interval: "3 more" counted from when the form opened, so 2 now
	assert.deepEqual(loopPatch(ticked, opened, { ...before, every: 2 }, 'p', NOW).schedule?.spec, 'DTSTART:20261110T233200Z\nRRULE:FREQ=MINUTELY;INTERVAL=2;COUNT=2');
	// nothing left: refused
	assert.throws(() => loopPatch({ ...ticked, ticks: 5 }, opened, { ...before, every: 2 }, 'p', NOW), /edit it again/);
});
