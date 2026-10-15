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
	import { page } from '$app/state';
	import { fly } from 'svelte/transition';
	import {
		TreeStructureIcon,
		ChatCircleIcon,
		FolderIcon,
		PathIcon,
		ScrollIcon,
		TerminalWindowIcon,
		DotsThreeOutlineIcon
	} from 'phosphor-svelte';
	import { logHref, recallSession } from '$lib/logView.svelte';
	import { serverInfo } from '$lib/serverInfo.svelte';
	import { viewport } from '$lib/viewport.svelte';
	import type { Component } from 'svelte';
	import type { IconComponentProps } from 'phosphor-svelte';
	import type { SessionDTO } from '$lib/types';

	type NavItem = {
		label: string;
		href: string;
		icon: Component<IconComponentProps>;
		active: boolean;
		secondary?: boolean;
	};

	let { runId, sessions = [] }: { runId: string; sessions?: SessionDTO[] } = $props();

	const base = $derived(`/runs/${runId}`);
	const path = $derived(page.url.pathname);

	// The two read-only viewers are per-session, so the rail needs a session to
	// point at: whichever you looked at last in this run, else a per-mode default.
	// The defaults differ because the modes suit different agents — the trajectory
	// is for the run's worker (the autonomous root), while a chat LOG is really
	// about an interactive session, today only the chatbot. Any session can still
	// be opened in either mode from the viewer's own selector; this is just where
	// the rail lands you first. Empty until the run detail arrives — logHref then
	// targets the index route, which redirects with the same rule.
	function defaultSid(mode: 'trajectory' | 'chat'): string {
		const remembered = recallSession(runId);
		if (remembered && sessions.some((s) => s.session_id === remembered)) return remembered;
		const roots = sessions.filter((s) => !s.parent_id);
		const autonomous = roots.find((s) => s.agent_type !== 'chatbot') ?? roots[0];
		if (mode === 'chat') {
			return (sessions.find((s) => s.agent_type === 'chatbot') ?? autonomous)?.session_id ?? '';
		}
		return autonomous?.session_id ?? '';
	}
	// A session-log route: /runs/<id>/sessions[/<sid>[/chat]]. The chat-log page
	// is the one ending in /chat — note the LIVE chat lives at /runs/<id>/chat and
	// never matches this prefix.
	const inLog = $derived(path.startsWith(`${base}/sessions`));
	const inLogChat = $derived(inLog && path.endsWith('/chat'));

	// The process view reads /proc, so it exists only when the SERVER runs Linux
	// (not the browser). Ask once; the entry stays hidden until the answer is
	// known, so it never appears and then vanishes under the cursor.
	serverInfo.load();

	// `secondary` is a phone-only hint: four thumb-sized tabs fit across 360px,
	// and these three are the wide, desktop-shaped views. The rail above 640px
	// still shows every entry.
	const items = $derived<NavItem[]>([
		{
			label: 'Overview',
			href: base,
			icon: TreeStructureIcon,
			active: path === base
		},
		{ label: 'Chat', href: `${base}/chat`, icon: ChatCircleIcon, active: path.startsWith(`${base}/chat`) },
		{
			label: 'Trajectory',
			href: logHref(runId, defaultSid('trajectory'), 'trajectory'),
			icon: PathIcon,
			active: inLog && !inLogChat,
			secondary: true
		},
		{
			label: 'Chat log',
			href: logHref(runId, defaultSid('chat'), 'chat'),
			icon: ScrollIcon,
			active: inLogChat,
			secondary: true
		},
		{
			label: 'Artifacts',
			href: `${base}/artifacts`,
			icon: FolderIcon,
			active: path.startsWith(`${base}/artifacts`)
		},
		...(serverInfo.isLinux
			? [
					{
						label: 'Processes',
						href: `${base}/processes`,
						icon: TerminalWindowIcon,
						active: path.startsWith(`${base}/processes`),
						secondary: true
					}
				]
			: [])
	]);

	const tabs = $derived(items.filter((it) => !it.secondary));
	const moreItems = $derived(items.filter((it) => it.secondary));
	let moreOpen = $state(false);
	// More carries the active tint when the current view lives inside it: the tab
	// bar is the only "where am I" indicator on a phone.
	const moreActive = $derived(moreItems.some((it) => it.active));
	// Rotating to landscape swaps the tab bar for the rail; close with it.
	$effect(() => {
		if (!viewport.isPhone) moreOpen = false;
	});
	// Wired only while open, so desktop carries no extra global key listener.
	$effect(() => {
		if (!moreOpen) return;
		const onKey = (e: KeyboardEvent) => {
			if (e.key === 'Escape') moreOpen = false;
		};
		window.addEventListener('keydown', onKey);
		return () => window.removeEventListener('keydown', onKey);
	});
</script>

<!-- Two shapes of the same nav, chosen in markup rather than CSS: the phone
     form drops entries into a sheet, adds a button among the links, and lives
     outside the layout flow. A CSS-only fork
     would have to ship the sheet's DOM to every desktop page to do that. -->
{#if viewport.isPhone}
	<nav class="tabbar" aria-label="Run sections">
		{#each tabs as it (it.href)}
			{@const Icon = it.icon}
			<a
				href={it.href}
				class="tab"
				class:active={it.active}
				aria-current={it.active ? 'page' : undefined}
				onclick={() => (moreOpen = false)}
			>
				<Icon size={22} weight={it.active ? 'fill' : 'regular'} />
				<span class="label">{it.label}</span>
			</a>
		{/each}
		<button
			class="tab"
			class:active={moreActive}
			aria-expanded={moreOpen}
			onclick={() => (moreOpen = !moreOpen)}
		>
			<DotsThreeOutlineIcon size={22} weight={moreActive || moreOpen ? 'fill' : 'regular'} />
			<span class="label">More</span>
		</button>
	</nav>
	{#if moreOpen}
		<!-- Scrim first in the DOM so the sheet paints over it. A real <button>
		     rather than a click-handling div, which keeps tap-outside-to-dismiss
		     reachable for
		     anyone driving this with a keyboard or a screen reader. -->
		<button class="scrim" aria-label="Close menu" onclick={() => (moreOpen = false)}></button>
		<div class="sheet" transition:fly={{ y: 120, duration: 150 }}>
			{#each moreItems as it (it.href)}
				{@const Icon = it.icon}
				<a
					href={it.href}
					class="sheet-item"
					class:active={it.active}
					aria-current={it.active ? 'page' : undefined}
					onclick={() => (moreOpen = false)}
				>
					<Icon size={20} weight={it.active ? 'fill' : 'regular'} />
					<span>{it.label}</span>
				</a>
			{/each}
			<!-- These views assume a wide window; say so before the tap. -->
			<p class="sheet-note dim small">These views are best on a wide screen.</p>
		</div>
	{/if}
{:else}
	<nav class="rail">
		{#each items as it (it.href)}
			{@const Icon = it.icon}
			<a
				href={it.href}
				class="item"
				class:active={it.active}
				aria-current={it.active ? 'page' : undefined}
			>
				<span class="ic"><Icon size={28} weight={it.active ? 'fill' : 'regular'} /></span>
				<span class="label">{it.label}</span>
			</a>
		{/each}
	</nav>
{/if}

<style>
	.rail {
		display: flex;
		flex-direction: column;
		gap: 0.4rem;
		width: 84px;
		flex-shrink: 0;
	}
	.item {
		display: flex;
		flex-direction: column;
		align-items: center;
		gap: 0.3rem;
		padding: 0.55rem 0;
		border-radius: var(--radius-lg);
		color: var(--text-dim);
		font-size: var(--fs-sm);
	}
	.item:hover {
		text-decoration: none;
		color: var(--text);
	}
	.item.active {
		color: var(--accent);
	}
	.ic {
		display: inline-flex;
		padding: 0.35rem 1rem;
		border-radius: var(--radius-pill);
	}
	.item:hover .ic {
		background: var(--bg-elev2);
	}
	.item.active .ic {
		background: color-mix(in srgb, var(--accent) 16%, transparent);
	}

	/* Phone: the rail becomes the bottom tab bar. Fixed, so a scrolling view
	   keeps its nav under the thumb; the space it covers is reserved by
	   <main>'s bottom padding via --bottom-nav-h,
	   which is also where its height is defined (app.css) so the two cannot
	   drift apart. Nothing below is reachable above 640px — the rail rules above
	   are untouched. */
	.tabbar {
		position: fixed;
		inset: auto 0 0 0;
		z-index: 20;
		display: flex;
		background: var(--bg-elev);
		border-top: 1px solid var(--border);
		/* Inset keeps labels off the home indicator; the fill still runs to the
		   edge, which is what makes the bar look anchored. */
		padding-bottom: env(safe-area-inset-bottom, 0px);
	}
	.tab {
		flex: 1;
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		gap: 0.1rem;
		min-height: 3.5rem; /* the 44px floor, with room for icon + label */
		padding: 0.35rem 0.2rem;
		color: var(--text-dim);
		font-size: var(--fs-xs);
		/* The More entry is a <button>; strip the global button chrome so it is
		   indistinguishable from the link tabs beside it. */
		background: none;
		border: none;
		border-radius: 0;
	}
	.tab.active {
		color: var(--accent);
	}
	.tab:hover {
		text-decoration: none;
	}
	.scrim {
		position: fixed;
		inset: 0;
		z-index: 19;
		background: color-mix(in srgb, var(--bg) 65%, transparent);
		border: none;
		border-radius: 0;
		padding: 0;
	}
	.sheet {
		position: fixed;
		/* Sits on the tab bar, not over it, so the sheet reads as part of More. */
		inset: auto 0 var(--bottom-nav-h) 0;
		z-index: 20;
		display: flex;
		flex-direction: column;
		background: var(--bg-elev);
		border-top: 1px solid var(--border);
		padding: 0.4rem 0;
	}
	.sheet-item {
		display: flex;
		align-items: center;
		gap: 0.7rem;
		min-height: 44px;
		padding: 0 1.1rem;
		color: var(--text);
	}
	.sheet-item:hover {
		text-decoration: none;
	}
	.sheet-item.active {
		color: var(--accent);
	}
	.sheet-note {
		margin: 0.3rem 1.1rem 0.2rem;
	}
</style>
