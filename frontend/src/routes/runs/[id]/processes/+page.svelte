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
	// What this run's bash calls left running, grouped by the agent that started
	// them. Agents background servers, watchers and test runs; until now nothing
	// in amplio could tell you what survived.
	//
	// POLLED, not pushed, and deliberately: the kernel has no event for an
	// arbitrary descendant forking or exiting, so this is a sample rather than a
	// stream. The page therefore shows the sample's age instead of implying it is
	// live, and stops polling when the tab is hidden — a scan costs real syscalls
	// and nobody should pay for a forgotten background tab.
	import { onDestroy } from 'svelte';
	import { page } from '$app/state';
	import { api, errorText } from '$lib/api';
	import { pageTitle } from '$lib/title';
	import { getRunStore } from '$lib/runContext.svelte';
	import { formatElapsed } from '$lib/time';
	import type { ProcessNode, ProcessSnapshot } from '$lib/types';
	import {
		ArrowClockwiseIcon,
		ClockIcon,
		CpuIcon,
		HashIcon,
		MemoryIcon,
		WarningIcon
	} from 'phosphor-svelte';

	// 5s, not 1-2s: a process list changes on human timescales, the scan costs
	// real syscalls, and Refresh covers "I want it now". The age beside the
	// heading ticks every second, so a slower cadence still reads as alive
	// rather than frozen.
	const POLL_MS = 5000;

	const runId = $derived(page.params.id ?? '');
	const store = getRunStore();
	const runLabel = $derived(store.detail?.title || store.detail?.task || runId);

	let snap = $state<ProcessSnapshot | null>(null);
	let error = $state('');
	// Only a CLICK disables the button. Reflecting background polls there made it
	// flicker once per tick, which reads as a glitch rather than as feedback.
	let refreshing = $state(false);
	let now = $state(Date.now()); // 1s ticker, so the sample age counts up between polls
	let timer: ReturnType<typeof setTimeout> | null = null;

	async function load(manual = false) {
		if (!runId) return;
		if (manual) refreshing = true;
		try {
			snap = await api.getProcesses(runId);
			error = '';
			now = Date.now();
		} catch (e) {
			error = errorText(e);
		} finally {
			if (manual) refreshing = false;
		}
	}

	// One timer, rescheduled after each response rather than a fixed interval, so
	// a slow scan can never stack requests on top of each other.
	function schedule() {
		if (timer) clearTimeout(timer);
		timer = setTimeout(async () => {
			if (!document.hidden && snap?.supported !== false) await load();
			schedule();
		}, POLL_MS);
	}

	$effect(() => {
		const id = runId;
		if (!id) return;
		load();
		schedule();
		const onVisible = () => {
			if (!document.hidden) load(); // catch up the moment the tab comes back
		};
		document.addEventListener('visibilitychange', onVisible);
		const tick = setInterval(() => {
			if (!document.hidden) now = Date.now();
		}, 1000);
		return () => {
			document.removeEventListener('visibilitychange', onVisible);
			clearInterval(tick);
			if (timer) clearTimeout(timer);
			timer = null;
		};
	});
	onDestroy(() => {
		if (timer) clearTimeout(timer);
	});

	// Grouped by the agent that started them. A process knows its own session
	// because the bash tool exports it into the environment, so this works even
	// for something whose launching shell died hours ago.
	type Group = { session: string; roots: ProcessNode[]; count: number };
	const groups = $derived.by<Group[]>(() => {
		const by = new Map<string, ProcessNode[]>();
		for (const r of snap?.roots ?? []) {
			const k = r.session_id || '(unknown session)';
			const list = by.get(k);
			if (list) list.push(r);
			else by.set(k, [r]);
		}
		return [...by.entries()]
			.map(([session, roots]) => ({ session, roots, count: roots.reduce(countTree, 0) }))
			.sort((a, b) => a.session.localeCompare(b.session));
	});
	function countTree(acc: number, p: ProcessNode): number {
		return acc + 1 + (p.children ?? []).reduce(countTree, 0);
	}


	function fmtBytes(n: number): string {
		if (n >= 1 << 30) return `${(n / (1 << 30)).toFixed(1)}G`;
		if (n >= 1 << 20) return `${Math.round(n / (1 << 20))}M`;
		return `${Math.round(n / 1024)}K`;
	}
	// CPU time keeps one decimal under 10s: the difference between 0.0s and 0.4s
	// is the difference between "asleep" and "doing something", and rounding to
	// a whole second erases it.
	function fmtCPU(ms: number): string {
		const sec = ms / 1000;
		return sec < 10 ? `${sec.toFixed(1)}s` : fmtDuration(sec);
	}
	function fmtDuration(sec: number): string {
		if (sec < 60) return `${Math.round(sec)}s`;
		if (sec < 3600) return `${Math.floor(sec / 60)}m${String(Math.round(sec % 60)).padStart(2, '0')}s`;
		const h = Math.floor(sec / 3600);
		return `${h}h${String(Math.floor((sec % 3600) / 60)).padStart(2, '0')}m`;
	}
	// Only the states worth a second look get a word; S (sleeping) is the normal
	// resting state of nearly everything and labelling it would be noise.
	function stateLabel(s: string): string {
		return { Z: 'zombie', D: 'uninterruptible', T: 'stopped', R: 'running' }[s] ?? '';
	}
</script>

<svelte:head><title>{pageTitle(`Processes · ${runLabel}`)}</title></svelte:head>

<div class="head">
	<h2>Processes</h2>
	{#if snap?.supported}
		<span class="dim small">
			{snap.total}
			{snap.total === 1 ? 'process' : 'processes'}
			{#if snap.total > 0}· sampled {formatElapsed(snap.taken_at, now)} ago in {snap.scan_millis}ms{/if}
		</span>
	{/if}
	<button class="refresh" onclick={() => load(true)} disabled={refreshing} title="Sample again now">
		<ArrowClockwiseIcon size={14} weight="bold" />
		Refresh
	</button>
</div>

{#if error}
	<p class="err">{error}</p>
{:else if snap && !snap.supported}
	<p class="dim">Process listing is not supported on {snap.platform}.</p>
{:else if !snap}
	<p class="dim small">Loading…</p>
{:else if snap.total === 0}
	<p class="dim">
		Nothing running. Processes appear here while an agent's bash call is working, and stay
		while anything it started outlives the call.
	</p>
{:else}
	{#each groups as g (g.session)}
		<section class="grp">
			<h3 class="grp-head mono">
				{g.session}
				<span class="dim small">{g.count}</span>
			</h3>
			{#each g.roots as r (r.pid)}
				{@render row(r, 0)}
			{/each}
		</section>
	{/each}
{/if}

{#snippet row(p: ProcessNode, depth: number)}
	{@const label = stateLabel(p.state)}
	<div class="proc" style="padding-left: {depth * 16}px">
		<span class="pid mono dim" title="Process id">
			<HashIcon size={11} weight="bold" />{p.pid}
		</span>
		{#if p.orphan}
			<!-- Its bash call has exited, so nothing will clean this up on timeout:
			     the case this page exists to surface. -->
			<span class="tag orphan" title="Reparented — the bash call that started it has exited">
				<WarningIcon size={11} weight="bold" /> orphan
			</span>
		{/if}
		{#if label && p.state !== 'R'}<span class="tag warn">{label}</span>{/if}
		<code class="cmd" title={p.cmdline}>{p.cmdline || '(no command line)'}</code>
		<!-- Three numbers, two of them durations: without the icons "3h04m · 2m58s ·
		     412M" is unreadable. Elapsed beside CPU time is the useful pair — a
		     process whose CPU time tracks its age is spinning, one whose CPU time
		     is flat is just sitting there. No percentage: over hours the totals
		     say more than an instantaneous rate, and a rate would need a second
		     sample and a paragraph of caveats. -->
		<span class="metrics mono dim small">
			<span class="metric" title="Elapsed since it started">
				<ClockIcon size={11} weight="bold" />{fmtDuration(p.elapsed_seconds)}
			</span>
			<span class="metric" title="CPU time consumed (user + system)">
				<CpuIcon size={11} weight="bold" />{fmtCPU(p.cpu_millis)}
			</span>
			<span class="metric" title="Resident memory">
				<MemoryIcon size={11} weight="bold" />{fmtBytes(p.rss_bytes)}
			</span>
		</span>
	</div>
	{#each p.children ?? [] as c (c.pid)}
		{@render row(c, depth + 1)}
	{/each}
{/snippet}

<style>
	.head {
		display: flex;
		align-items: baseline;
		gap: 0.6rem;
		margin-bottom: 0.8rem;
	}
	h2 {
		margin: 0;
		font-size: var(--fs-lg);
	}
	.refresh {
		margin-left: auto;
		display: inline-flex;
		align-items: center;
		gap: 0.3rem;
		font-size: var(--fs-sm);
		padding: 0.25rem 0.6rem;
		border: 1px solid var(--border);
		border-radius: var(--radius-pill);
		background: transparent;
		color: var(--text-dim);
		cursor: pointer;
	}
	.refresh:hover:not(:disabled) {
		color: var(--text);
		background: var(--bg-elev2);
	}
	.grp {
		margin-bottom: 1.1rem;
	}
	.grp-head {
		display: flex;
		align-items: baseline;
		gap: 0.5rem;
		margin: 0 0 0.35rem;
		font-size: var(--fs-sm);
		font-weight: 500;
	}
	.proc {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		padding: 0.25rem 0.5rem;
		border-radius: var(--radius-sm);
	}
	.proc:hover {
		background: var(--bg-elev2);
	}
	.pid {
		flex-shrink: 0;
		display: inline-flex;
		align-items: center;
		gap: 0.15rem;
		font-size: var(--fs-sm);
		min-width: 5.2rem;
	}
	.metric {
		display: inline-flex;
		align-items: center;
		gap: 0.2rem;
		min-width: 4.2rem; /* columns line up down the list instead of ragging */
		justify-content: flex-end;
	}
	.cmd {
		flex: 1 1 0;
		min-width: 0;
		overflow: hidden;
		white-space: nowrap;
		font-size: var(--fs-sm);
		mask-image: linear-gradient(to right, black calc(100% - 1.5rem), transparent);
		-webkit-mask-image: linear-gradient(to right, black calc(100% - 1.5rem), transparent);
	}
	.metrics {
		flex-shrink: 0;
		display: inline-flex;
		align-items: center;
		gap: 0.7rem;
	}
	.tag {
		flex-shrink: 0;
		display: inline-flex;
		align-items: center;
		gap: 0.2rem;
		font-size: var(--fs-xs);
		padding: 0.05rem 0.4rem;
		border-radius: var(--radius-pill);
		border: 1px solid var(--border);
		color: var(--text-dim);
	}
	.tag.orphan {
		color: var(--warn, #e0a030);
		border-color: currentColor;
	}
	.tag.warn {
		color: var(--danger, #e06c6c);
		border-color: currentColor;
	}
	.err {
		color: var(--danger, #e06c6c);
	}
</style>
