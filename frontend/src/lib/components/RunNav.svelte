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
	import { RowsIcon, BookOpenIcon } from 'phosphor-svelte';
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
	import { runMenu } from '$lib/runMenu.svelte';
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

	// Rotating to landscape swaps the drawer for the rail; close with it.
	$effect(() => {
		if (!viewport.isPhone) runMenu.open = false;
	});
	// Wired only while open, so desktop carries no extra global key listener.
	$effect(() => {
		if (!runMenu.open) return;
		const onKey = (e: KeyboardEvent) => {
			if (e.key === 'Escape') runMenu.open = false;
		};
		window.addEventListener('keydown', onKey);
		return () => window.removeEventListener('keydown', onKey);
	});
</script>

<!-- Two shapes of the same nav, chosen in markup rather than CSS: the phone
     form is a drawer opened from the run bar, not a restyled rail, and a
     CSS-only fork would ship the drawer's DOM to every desktop page. -->
{#if viewport.isPhone}
	{#if runMenu.open}
		<!-- Scrim first in the DOM so the drawer paints over it. A real <button>
		     rather than a click-handling div, which keeps tap-outside-to-dismiss
		     reachable for anyone driving this with a keyboard or a screen reader. -->
		<button class="scrim" aria-label="Close menu" onclick={() => (runMenu.open = false)}></button>
		<nav class="drawer" aria-label="Run sections" transition:fly={{ x: -200, duration: 150 }}>
			<p class="drawer-head dim small">This run</p>
			{#each items as it (it.href)}
				{@const Icon = it.icon}
				<a
					href={it.href}
					class="drawer-item"
					class:active={it.active}
					aria-current={it.active ? 'page' : undefined}
					onclick={() => (runMenu.open = false)}
				>
					<Icon size={20} weight={it.active ? 'fill' : 'regular'} />
					<span>{it.label}</span>
					{#if it.secondary}<span class="wide-hint dim small">wide</span>{/if}
				</a>
			{/each}
			<div class="drawer-sep" role="separator"></div>
			<a class="drawer-item" href="/" onclick={() => (runMenu.open = false)}>
				<RowsIcon size={20} />
				<span>All Runs</span>
			</a>
			<a class="drawer-item" href="/recall" onclick={() => (runMenu.open = false)}>
				<BookOpenIcon size={20} />
				<span>Recall</span>
			</a>
		</nav>
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

	/* Phone: no bottom bar at all — it sat on top of the composer in chat. The
	   rail's entries move into a left drawer opened from the run bar, so the
	   bottom of the screen belongs to the view. */
	@media (max-width: 640px) {
		.scrim {
			position: fixed;
			inset: 0;
			z-index: 29;
			background: color-mix(in srgb, var(--bg) 65%, transparent);
			border: none;
			border-radius: 0;
			padding: 0;
		}
		.drawer {
			position: fixed;
			inset: 0 auto 0 0;
			z-index: 30;
			width: min(17rem, 82vw);
			padding: 0.6rem 0.5rem calc(0.6rem + env(safe-area-inset-bottom, 0px));
			display: flex;
			flex-direction: column;
			gap: 0.15rem;
			overflow-y: auto;
			background: var(--bg-elev);
			border-right: 1px solid var(--border);
		}
		.drawer-head {
			text-transform: uppercase;
			letter-spacing: 0.04em;
			margin: 0.3rem 0.6rem 0.2rem;
		}
		.drawer-item {
			display: flex;
			align-items: center;
			gap: 0.6rem;
			min-height: 44px;
			padding: 0 0.6rem;
			border-radius: var(--radius-sm);
			color: var(--text);
		}
		.drawer-item:hover {
			text-decoration: none;
			background: var(--bg-elev2);
		}
		.drawer-item.active {
			color: var(--accent);
			background: color-mix(in srgb, var(--accent) 12%, transparent);
		}
		/* These views assume a wide window; say so before the tap. */
		.wide-hint {
			margin-left: auto;
		}
		.drawer-sep {
			height: 1px;
			margin: 0.4rem 0.6rem;
			background: var(--border);
		}
	}
</style>
