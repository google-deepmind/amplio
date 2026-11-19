<!--
 Copyright 2026 Google LLC

 Licensed under the Apache License, Version 2.0 (the "License");
 you may not use this file except in compliance with the License.
 You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
-->

<script lang="ts">
	// The Inspector: what keeps a run's agents working, and what they left
	// running. Pick a session (?session=) or the whole run; then Verification,
	// Loops and Processes. Reads are open; changing verification or loops needs
	// the token, and the readonly view shows the same information without the
	// controls. The forms are the ones the composer and the new-run form use.
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, errorText } from '$lib/api';
	import { getRunStore } from '$lib/runContext.svelte';
	import { pageTitle } from '$lib/title';
	import { serverSettings } from '$lib/serverSettings.svelte';
	import { describeSpec, formatWhen } from '$lib/loopSchedule';
	import { roundsText } from '$lib/composerOptions';
	import { roundOf } from '$lib/verifyText';
	import { activeLoops, shownVerify, stateMarks, verifyStatusText } from '$lib/sessionState';
	import { coalescer, Latest } from '$lib/refetch';
	import { viewport } from '$lib/viewport.svelte';
	import type { LoopDTO, SessionDTO, VerifyReport } from '$lib/types';
	import SessionTree from '$lib/components/SessionTree.svelte';
	import InspectSection from '$lib/components/InspectSection.svelte';
	import VerifyPanel from '$lib/components/VerifyPanel.svelte';
	import LoopsPanel from '$lib/components/LoopsPanel.svelte';
	import ProcessList from '$lib/components/ProcessList.svelte';
	import { ListMagnifyingGlassIcon, RepeatIcon, SealCheckIcon, TerminalWindowIcon } from 'phosphor-svelte';

	const store = getRunStore();
	const runId = $derived(page.params.id ?? '');
	const sessions = $derived(store.detail?.sessions ?? []);
	const sid = $derived(page.url.searchParams.get('session') ?? '');
	const session = $derived(sessions.find((s) => s.session_id === sid));
	const runLabel = $derived(store.detail?.title || store.detail?.task || runId);
	const zone = $derived(serverSettings.displayZone);

	// What the view loads, per session. A session's view: its verification
	// report (state and history) and its loops. The whole run's: only the loops
	// of the sessions whose summary in the run detail says they have some; its
	// verification rows read those summaries, so it never reads every
	// session's stream. A chatbot never concludes and a verifier isn't
	// verified, so neither's verification is asked for.
	type Part = 'verify' | 'loops';
	const verifiable = (s: SessionDTO) => !s.interactive && !s.verifier_for;
	const needs = $derived.by(() => {
		const out = new Map<string, Part[]>();
		if (sid) {
			if (session) out.set(sid, verifiable(session) ? ['verify', 'loops'] : ['loops']);
		} else {
			for (const s of sessions) if (activeLoops(s)) out.set(s.session_id, ['loops']);
		}
		return out;
	});

	// What was loaded, kept across picks.
	let reports = $state<Record<string, VerifyReport | null>>({});
	let loops = $state<Record<string, LoopDTO[] | null>>({});
	// Parts whose last load failed, by partKey (the sections say so; the next
	// event retries).
	let failed = $state<Record<string, string>>({});
	const partKey = (part: Part, id: string) => `${part}/${id}`;
	const loaded = (part: Part, id: string) => (part === 'verify' ? reports[id] : loops[id]) !== undefined;

	// A part is loaded when the view needs it, unless what is kept for it is
	// current, and again on its session's events. fresh holds the parts whose
	// kept data is current or being fetched: an event for a session the view
	// doesn't need only takes its parts out, so they reload when it needs them
	// again. latest drops a response that a newer request, or a run switch, has
	// overtaken.
	const fresh = new Set<string>();
	const latest = new Latest();
	async function loadPart(id: string, part: Part) {
		const k = partKey(part, id);
		const current = latest.start(k);
		const run = runId;
		fresh.add(k);
		try {
			if (part === 'verify') {
				const r = await api.getVerify(run, id);
				if (!current()) return;
				reports[id] = r;
			} else {
				const l = await api.listLoops(run, id);
				if (!current()) return;
				loops[id] = l;
			}
			delete failed[k];
		} catch (e) {
			if (!current()) return;
			fresh.delete(k);
			failed[k] = errorText(e) || 'the server is unreachable';
		}
	}
	function load(id: string) {
		for (const part of needs.get(id) ?? []) void loadPart(id, part);
	}

	// A change shows up as the session's new events (a VerifyEvent, a LoopEvent,
	// a tick, a verdict), and a working agent appends one every step. One
	// session's view refetches 300 ms after a burst; the whole run's, at most
	// every 5 s per session with loops. A change of view doesn't cancel a
	// pending refetch: the session's data is kept either way.
	const refetchOne = coalescer(load, 300, 'debounce');
	const refetchAll = coalescer(load, 5000, 'throttle');
	function cancelRefetches() {
		refetchOne.cancel();
		refetchAll.cancel();
	}

	let shownRun = '';
	$effect(() => {
		if (runId !== shownRun) {
			shownRun = runId;
			cancelRefetches();
			fresh.clear();
			latest.reset();
			reports = {};
			loops = {};
			failed = {};
		}
		for (const [id, parts] of needs) {
			for (const part of parts) if (!fresh.has(partKey(part, id))) void loadPart(id, part);
		}
	});
	$effect(() => {
		const changed = (id: string) => {
			if (needs.has(id)) (sid ? refetchOne : refetchAll).schedule(id);
			else {
				fresh.delete(partKey('verify', id));
				fresh.delete(partKey('loops', id));
			}
		};
		const off = store.on((ev) => {
			if (ev.kind === 'refetch_all') {
				const ids = new Set([...needs.keys()]);
				for (const k of fresh) ids.add(k.slice(k.indexOf('/') + 1));
				for (const id of ids) changed(id);
			} else if (ev.session_id && (ev.kind === 'session_bump' || ev.kind === 'status_change')) {
				changed(ev.session_id);
			}
		});
		return () => {
			off();
			cancelRefetches();
		};
	});

	// The session, or the nearest one above it, that is cancelled: no loop can
	// be added there (a tick must not revive it).
	const cancelled = $derived.by(() => {
		for (let s = session, d = 0; s && d < 64; d++) {
			if (s.status === 'cancelled') return s.session_id;
			s = sessions.find((x) => x.session_id === s?.parent_id);
		}
		return '';
	});

	const hrefFor = (s: SessionDTO) => `/runs/${runId}/inspector?session=${encodeURIComponent(s.session_id)}`;
	const runHref = $derived(`/runs/${runId}/inspector`);

	// The whole run's rows: sessions whose verification isn't off, from their
	// summaries, and every loop of the sessions that have some.
	const verifyRows = $derived(
		sessions.flatMap((s) => {
			const v = shownVerify(s);
			return v ? [{ s, v }] : [];
		})
	);
	const loopRows = $derived(
		sessions.flatMap((s) => (needs.has(s.session_id) ? (loops[s.session_id] ?? []).map((l) => ({ s, l })) : []))
	);
	// Until the run's detail arrives there is nothing to want, which must read
	// as loading, not as "nothing here".
	const loopsLoading = $derived(
		!store.detail ||
			[...needs].some(([id, parts]) => parts.includes('loops') && !loaded('loops', id) && !failed[partKey('loops', id)])
	);
	const failures = $derived(
		Object.entries(failed).flatMap(([k, msg]) => {
			const [part, id] = [k.slice(0, k.indexOf('/')) as Part, k.slice(k.indexOf('/') + 1)];
			return needs.get(id)?.includes(part) ? [{ id, part, msg }] : [];
		})
	);
	// A section whose load failed with nothing kept shows the failure instead.
	const lost = (part: Part, id: string) => !!failed[partKey(part, id)] && !loaded(part, id);

	// Phone: a select instead of the tree, indented by depth.
	function depthOf(s: SessionDTO): number {
		let d = 0;
		for (let p = s.parent_id; p && d < 32; d++) p = sessions.find((x) => x.session_id === p)?.parent_id ?? '';
		return d;
	}
	function ordered(): SessionDTO[] {
		const out: SessionDTO[] = [];
		const walk = (parent: string) => {
			for (const s of sessions.filter((x) => (x.parent_id ?? '') === parent)) {
				out.push(s);
				walk(s.session_id);
			}
		};
		walk('');
		return out;
	}
</script>

<svelte:head><title>{pageTitle(sid ? `${sid} · inspector` : 'Inspector', runLabel)}</title></svelte:head>

<div class="insp">
	<header class="insp-head">
		<ListMagnifyingGlassIcon size={20} weight="bold" />
		<h2>Inspector</h2>
		<span class="dim small">verification, loops and processes{sid ? ` of ${sid}` : ', for the whole run'}</span>
	</header>

	<div class="cols">
		<nav class="picker" aria-label="Session">
			{#if viewport.isPhone}
				<select
					aria-label="Session"
					value={sid}
					onchange={(e) => goto(e.currentTarget.value ? hrefFor({ session_id: e.currentTarget.value } as SessionDTO) : runHref)}
				>
					<option value="">Whole run</option>
					{#each ordered() as s (s.session_id)}
						<option value={s.session_id}>{'\u00a0\u00a0'.repeat(depthOf(s))}{stateMarks(s)}{s.session_id} · {s.status}</option>
					{/each}
				</select>
			{:else}
				<a class="whole" class:selected={!sid} aria-current={!sid ? 'true' : undefined} href={runHref}>Whole run</a>
				{#if sessions.length}
					<SessionTree {runId} {sessions} variant="selector" selectedId={sid} {hrefFor} />
				{:else}
					<p class="dim small">Loading sessions…</p>
				{/if}
			{/if}
		</nav>

		<div class="sections">
			{#if failures.length}
				<p class="err small" role="alert">
					Couldn't load {[...new Set(failures.map((f) => f.id))].join(', ')}: {failures[0].msg}. It retries on the session's next event.
				</p>
			{/if}
			{#if sid && !session}
				<p class="dim">{store.detail ? `No session ${sid} in this run.` : 'Loading…'}</p>
			{:else if session}
				<!-- New panels per session: an open form, or a save in flight, belongs to the session it was opened on. -->
				{#key sid}
					{#if !lost('verify', sid)}
						<div id="verification">
							<VerifyPanel {runId} {session} report={reports[sid]} onreport={(id, r) => (reports[id] = r)} />
						</div>
					{/if}
					{#if !lost('loops', sid)}
						<div id="loops">
							<LoopsPanel {runId} {session} {cancelled} loops={loops[sid]} onloops={(id, l) => (loops[id] = l)} />
						</div>
					{/if}
				{/key}
				<InspectSection title="Processes" icon={TerminalWindowIcon}>
					<ProcessList {runId} session={sid} />
				</InspectSection>
			{:else}
				<InspectSection title="Verification" icon={SealCheckIcon}>
					{#if !store.detail}
						<p class="dim small">Loading…</p>
					{:else if verifyRows.length === 0}
						<p class="dim small">No session has verification on, passed or exhausted. Pick a session to turn it on.</p>
					{:else}
						<ul class="rows">
							{#each verifyRows as { s, v } (s.session_id)}
								<li>
									<a class="mono" href={hrefFor(s)}>{s.session_id}</a>
									<span class="vstate vstate-{v.status}">{verifyStatusText(v.status)}</span>
									<span class="dim small">
										{#if (v.status === 'verifying' || v.status === 'on') && v.round}{roundOf(v.round, v.max_rounds ?? 0)}{/if}
										{#if v.verifier}· {v.verifier}{/if}
									</span>
								</li>
							{/each}
						</ul>
					{/if}
				</InspectSection>
				<InspectSection title="Loops" icon={RepeatIcon}>
					{#if loopsLoading}
						<p class="dim small">Loading…</p>
					{:else if loopRows.length === 0}
						<p class="dim small">No active loops. Pick a session to add one.</p>
					{:else}
						<ul class="rows">
							{#each loopRows as { s, l } (s.session_id + '/' + l.id)}
								<li>
									<a class="mono" href={hrefFor(s)}>{s.session_id}</a>
									<span>{describeSpec(l.schedule.spec, l.summary, zone, serverSettings.now())}</span>
									<span class="dim small">
										loop {l.id} · {l.ticks} {l.ticks === 1 ? 'tick' : 'ticks'}
										{#if l.next_tick}· next {formatWhen(Date.parse(l.next_tick), zone, serverSettings.now())}{/if}
										{#if l.verify}· <span class="each-tick">verifies each tick ({roundsText(l.verify.max_rounds)})</span>{/if}
									</span>
									<span class="prompt dim small">{l.prompt}</span>
								</li>
							{/each}
						</ul>
					{/if}
				</InspectSection>
				<div id="processes">
					<InspectSection title="Processes" icon={TerminalWindowIcon}>
						<ProcessList {runId} />
					</InspectSection>
				</div>
			{/if}
		</div>
	</div>
</div>

<style>
	.insp {
		display: flex;
		flex-direction: column;
		gap: 0.8rem;
		min-height: 0;
	}
	.insp-head {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		flex-wrap: wrap;
	}
	h2 {
		margin: 0;
		font-size: var(--fs-lg);
	}
	.cols {
		display: flex;
		gap: 1rem;
		align-items: flex-start;
	}
	.picker {
		flex: 0 0 19rem;
		display: flex;
		flex-direction: column;
		gap: 0.2rem;
		position: sticky;
		top: 0;
		max-height: 80dvh;
		overflow-y: auto;
	}
	.whole {
		padding: 0.3rem 0.5rem;
		border-radius: var(--radius-sm);
		color: var(--text);
	}
	.whole.selected {
		background: color-mix(in srgb, var(--accent) 14%, transparent);
		color: var(--accent);
	}
	.sections {
		flex: 1 1 0;
		min-width: 0;
		display: flex;
		flex-direction: column;
		gap: 0.8rem;
	}
	.rows {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.4rem;
	}
	.rows li {
		display: flex;
		flex-wrap: wrap;
		align-items: baseline;
		gap: 0.2rem 0.6rem;
	}
	.prompt {
		flex-basis: 100%;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.each-tick {
		color: var(--accent);
	}
	.vstate {
		font-size: var(--fs-sm);
		padding: 0 0.5rem;
		border-radius: var(--radius-pill);
		border: 1px solid var(--border);
		color: var(--text-dim);
	}
	.vstate-on,
	.vstate-verifying {
		color: var(--accent);
	}
	.vstate-passed {
		color: var(--ok);
	}
	.vstate-not_verified {
		color: var(--warn);
	}
	.err {
		margin: 0;
	}
	@media (max-width: 640px) {
		.cols {
			flex-direction: column;
			align-items: stretch;
		}
		.picker {
			position: static;
			flex: none;
			max-height: none;
		}
	}
</style>
