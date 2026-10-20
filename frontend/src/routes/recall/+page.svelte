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
	import { tick } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api } from '$lib/api';
	import { viewport } from '$lib/viewport.svelte';
	import { renderMarkdown } from '$lib/markdown';
	import { pageTitle } from '$lib/title';
	import { CaretLeftIcon } from 'phosphor-svelte';
	import type { RecallResults, RecallItem, LessonSummary } from '$lib/types';

	let query = $state('');
	let results = $state<RecallResults | null>(null);
	let searching = $state(false);
	let timer: ReturnType<typeof setTimeout> | undefined;

	let lessons = $state<LessonSummary[]>([]);

	// ?entry=<handle> is what's open, as ?file= is on the artifacts page: deep
	// links work, and on phone (where the reader replaces the list) opening is a
	// real history entry, so Back closes the reader whatever overlays came and
	// went above it.
	const selected = $derived(page.url.searchParams.get('entry') ?? '');
	let item = $state<RecallItem | null>(null);
	let loadingItem = $state(false);

	$effect(() => {
		void loadLessons();
		// Clear any pending debounce on teardown so a search can't fire into a
		// destroyed component after navigation.
		return () => clearTimeout(timer);
	});

	async function loadLessons() {
		try {
			lessons = await api.listLessons();
		} catch {
			lessons = [];
		}
	}

	function onInput() {
		clearTimeout(timer);
		const q = query.trim();
		if (!q) {
			results = null;
			return;
		}
		timer = setTimeout(() => void runSearch(q), 250);
	}

	async function runSearch(q: string) {
		searching = true;
		try {
			const r = await api.searchRecall(q);
			// A slower search must not clobber a newer query's results.
			if (q !== query) return;
			results = r;
		} catch {
			if (q === query) results = null;
		} finally {
			if (q === query) searching = false;
		}
	}

	const reading = $derived(viewport.isPhone && !!selected);
	let recallEl = $state<HTMLElement>();
	let listScroll = 0;
	const scroller = () => recallEl?.closest('main');
	$effect(() => {
		const r = reading;
		void tick().then(() => {
			const m = scroller();
			if (m) m.scrollTop = r ? 0 : listScroll;
		});
	});

	// Built from location, not page.url: auth strips ?token= behind the router.
	function entryUrl(handle: string): URL {
		const url = new URL(location.href);
		if (handle) url.searchParams.set('entry', handle);
		else url.searchParams.delete('entry');
		return url;
	}
	let pushed = ''; // the entry this page pushed (Back returns to the list)

	function select(handle: string) {
		if (viewport.isPhone) listScroll = scroller()?.scrollTop ?? 0;
		// Desktop shows list and reader side by side: picking replaces the entry.
		const push = viewport.isPhone && !selected;
		if (push) pushed = handle;
		void goto(entryUrl(handle), { replaceState: !push, noScroll: true, keepFocus: true });
	}

	function closeReader() {
		if (pushed && pushed === selected) history.back();
		else void goto(entryUrl(''), { replaceState: true, noScroll: true, keepFocus: true });
	}

	$effect(() => {
		const handle = selected;
		if (!handle) {
			pushed = '';
			item = null;
			return;
		}
		void loadItem(handle);
	});

	async function loadItem(handle: string) {
		loadingItem = true;
		item = null;
		try {
			const it = await api.getRecallItem(handle);
			// A slower item-load must not clobber a newer selection.
			if (handle !== selected) return;
			item = it;
		} catch (e) {
			// Derive kind from the handle prefix so a lesson's error doesn't render
			// as a skill.
			if (handle === selected) {
				item = handle.startsWith('lesson:')
					? { kind: 'lesson', body: `Error: ${String(e)}` }
					: { kind: 'skill', body: `Error: ${String(e)}` };
			}
		} finally {
			if (handle === selected) loadingItem = false;
		}
	}

	// signedScore renders a lesson's attribution score with an explicit sign so
	// the polarity is unambiguous: +3 (helpful), -3 (harmful), 0 (neutral). The
	// score is a signed counter accumulated from per-run verdicts.
	function signedScore(n: number | undefined): string {
		const v = n ?? 0;
		return v > 0 ? `+${v}` : String(v);
	}
</script>

<svelte:head>
	<title>{pageTitle('Recall')}</title>
</svelte:head>

<h2>Recall</h2>
<p class="dim sub">The skill + lesson corpus agents search via recall. Type to rank like an agent; click any entry to read its full body.</p>

<div class="recall" class:reading bind:this={recallEl}>
	<div class="left card">
		<input
			class="search"
			type="search"
			placeholder="Search skills + lessons…"
			bind:value={query}
			oninput={onInput}
		/>

		{#if results}
			<div class="section">
				<h3>Skills{searching ? ' …' : ` (${results.skills.length})`}</h3>
				{#if results.skills.length === 0}
					<p class="dim empty">No matching skills.</p>
				{/if}
				{#each results.skills as h (h.handle)}
					<button class="row" class:active={selected === h.handle} onclick={() => select(h.handle)}>
						<span class="name">{h.name}</span>
						<span class="desc dim">{h.description}</span>
					</button>
				{/each}
			</div>
			<div class="section">
				<h3>Lessons ({results.lessons.length})</h3>
				{#if results.lessons.length === 0}
					<p class="dim empty">No matching lessons.</p>
				{/if}
				{#each results.lessons as h (h.handle)}
					<button class="row" class:active={selected === h.handle} onclick={() => select(h.handle)}>
						<span class="name">{h.title} <span class="badge mono">score {signedScore(h.score)} · {h.loaded_count}×</span></span>
						<span class="desc dim">{h.description}</span>
					</button>
				{/each}
			</div>
		{/if}

		<div class="section">
			<h3>All lessons ({lessons.length})</h3>
			{#if lessons.length === 0}
				<p class="dim empty">No lessons mined yet — they appear as runs conclude.</p>
			{/if}
			{#each lessons as l}
				<button class="row" class:active={selected === `lesson:${l.id}`} onclick={() => select(`lesson:${l.id}`)}>
					<span class="name">{l.title} <span class="badge mono">score {signedScore(l.score)} · {l.loaded_count}×</span></span>
					<span class="desc dim">{l.description}</span>
				</button>
			{/each}
		</div>
	</div>

	<div class="right card">
		{#if reading}
			<button class="back" onclick={closeReader}><CaretLeftIcon size={16} />All entries</button>
		{/if}
		{#if loadingItem}
			<p class="dim">Loading…</p>
		{:else if item}
			<div class="item">
				<h3>{item.kind === 'skill' ? item.name : item.title}</h3>
				<p class="meta dim mono">
					{#if item.kind === 'skill'}skill · {item.path}{:else}lesson:{item.id} · score {signedScore(item.score)} · used {item.loaded_count}× · source {item.source_run || 'unknown'}{/if}
				</p>
				{#if item.description}<p class="idesc">{item.description}</p>{/if}
				<div class="md">{@html renderMarkdown(item.body)}</div>
			</div>
		{:else}
			<p class="dim">Select a skill or lesson to read it.</p>
		{/if}
	</div>
</div>

<style>
	h2 {
		margin: 0 0 0.2rem;
	}
	.sub {
		margin: 0 0 1rem;
		font-size: var(--fs-md);
	}
	.recall {
		display: grid;
		grid-template-columns: minmax(0, 1fr) minmax(0, 1.3fr);
		gap: 1rem;
		align-items: start;
	}
	.left,
	.right {
		min-height: 0;
	}
	.search {
		width: 100%;
		margin-bottom: 0.6rem;
	}
	.section {
		margin-top: 0.8rem;
	}
	.section h3 {
		margin: 0 0 0.4rem;
		font-size: var(--fs-md);
		color: var(--text-dim);
		text-transform: uppercase;
		letter-spacing: 0.04em;
	}
	.empty {
		font-size: var(--fs-md);
		margin: 0.2rem 0;
	}
	.row {
		display: flex;
		flex-direction: column;
		gap: 0.1rem;
		width: 100%;
		text-align: left;
		background: none;
		border: none;
		border-radius: var(--radius-sm);
		padding: 0.35rem 0.5rem;
		cursor: pointer;
		color: var(--text);
	}
	.row:hover {
		background: var(--bg-elev);
	}
	.row.active {
		background: var(--bg-elev);
		box-shadow: inset 2px 0 0 var(--accent);
	}
	.name {
		font-size: var(--fs-md);
		font-weight: 500;
	}
	.badge {
		font-size: var(--fs-xs);
		color: var(--text-dim);
		font-weight: 400;
	}
	.desc {
		font-size: var(--fs-sm);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.right {
		position: sticky;
		top: 1rem;
	}
	.item h3 {
		margin: 0 0 0.3rem;
	}
	.meta {
		margin: 0 0 0.6rem;
		font-size: var(--fs-sm);
		word-break: break-all;
	}
	.idesc {
		margin: 0 0 0.8rem;
		font-style: italic;
		color: var(--text-dim);
	}

	@media (max-width: 640px) {
		h2 {
			font-size: var(--fs-xl);
		}
		h2:has(~ .recall.reading),
		.sub:has(~ .recall.reading) {
			display: none;
		}
		/* Side by side at 390px left the list 162px wide. */
		.recall {
			grid-template-columns: minmax(0, 1fr);
		}
		.right {
			position: static;
		}
		.recall:not(.reading) .right,
		.recall.reading .left {
			display: none;
		}
		.back {
			display: inline-flex;
			align-items: center;
			gap: var(--sp-1);
			margin: calc(-1 * var(--sp-3)) 0 var(--sp-1) calc(-1 * var(--sp-3));
			padding: 0 var(--sp-3);
			background: none;
			border: none;
			color: var(--accent);
			font-size: var(--fs-md);
		}
		/* Rows keep a pressed/active inset, and their text still lines up with
		   the search box and section headings. */
		.left {
			padding-inline: var(--sp-1);
		}
		.left .search {
			width: calc(100% - 2 * var(--sp-2));
			margin-inline: var(--sp-2);
		}
		.section h3,
		.empty {
			padding-inline: var(--sp-2);
		}
		.row {
			padding: var(--sp-1) var(--sp-2);
			justify-content: center;
		}
		.name {
			overflow: hidden;
			text-overflow: ellipsis;
			white-space: nowrap;
		}
	}
</style>
