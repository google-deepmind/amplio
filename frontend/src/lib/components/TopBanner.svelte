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
	import {
		RowsIcon,
		PulseIcon,
		BellIcon,
		BookOpenIcon,
		EyeIcon,
		InfoIcon,
		ListIcon,
		XIcon
	} from 'phosphor-svelte';
	import { fly } from 'svelte/transition';
	import { runsStore } from '$lib/runsStore.svelte';
	import { updates } from '$lib/updates.svelte';
	import { auth } from '$lib/auth.svelte';
	import { sysstat } from '$lib/sysstat.svelte';
	import { viewport } from '$lib/viewport.svelte';
	import { GcertChip, INTERNAL } from './internal';
	import CpuChip from './CpuChip.svelte';
	import MemChip from './MemChip.svelte';
	import SwapChip from './SwapChip.svelte';

	// Global, app-wide banner. Holds branding + global navigation on the left
	// (three discoverable filter targets), and the global status cluster on
	// the right (cpu/mem/swap/gcert). Run-scoped info belongs in the run
	// header below it, not here — keeps this strip stable across navigation.
	//
	// Nav items are stable (always present) so layout never shifts as runs
	// come and go; only the (N) suffix appears/disappears with the count.

	// The row holds the brand OR the nav, not both. Chips stay (they are the
	// reason to glance here); the nav entries move into a drawer.
	const activeCount = $derived(runsStore.activeCount);
	const unreadCount = $derived(updates.count);
	let drawerOpen = $state(false);
	// Phone-only; drop it on the way out so rotating back doesn't restore it.
	$effect(() => {
		if (!viewport.isPhone) drawerOpen = false;
	});
	// Wired only while open, so desktop carries no extra global key listener.
	$effect(() => {
		if (!drawerOpen) return;
		const onKey = (e: KeyboardEvent) => {
			if (e.key === 'Escape') drawerOpen = false;
		};
		window.addEventListener('keydown', onKey);
		return () => window.removeEventListener('keydown', onKey);
	});
</script>

<header class="bar">
	<!-- Edge-to-edge background (the .bar) with the actual contents constrained
	     to the same column as <main> below (the .inner). On wide displays the
	     brand + status chips stay aligned with the content column instead of
	     drifting to the screen corners; on narrower screens both layers
	     collapse to the same width so the seam is invisible. -->
	<!-- One definition of the nav entries, rendered either inline (desktop) or
	     inside the phone drawer. Two copies would drift the moment someone adds
	     a destination. -->
	{#snippet navItems()}
		<a href="/" class="nav-item" onclick={() => (drawerOpen = false)}>
			<RowsIcon size={14} weight="bold" />
			<span>All Runs</span>
		</a>
		<a
			href="/?filter=active"
			class="nav-item"
			title="{activeCount} run(s) not yet terminal"
			onclick={() => (drawerOpen = false)}
		>
			<PulseIcon size={14} weight="bold" />
			<span>Active Runs</span>
			{#if activeCount > 0}
				<span class="count count-active">({activeCount})</span>
			{/if}
		</a>
		<a
			href="/?filter=updates"
			class="nav-item"
			title="{unreadCount} run(s) with unseen updates"
			onclick={() => (drawerOpen = false)}
		>
			<BellIcon size={14} weight="bold" />
			<span>Recent Updates</span>
			{#if unreadCount > 0}
				<span class="count count-unread">({unreadCount})</span>
			{/if}
		</a>
		<a href="/recall" class="nav-item" onclick={() => (drawerOpen = false)}>
			<BookOpenIcon size={14} weight="bold" />
			<span>Recall</span>
		</a>
		<a href="/about" class="nav-item" onclick={() => (drawerOpen = false)}>
			<InfoIcon size={14} weight="bold" />
			<span>About</span>
		</a>
	{/snippet}
	<div class="inner">
		<div class="left">
			{#if viewport.isPhone}
				<button
					class="menu-toggle"
					aria-label={drawerOpen ? 'Close menu' : 'Open menu'}
					aria-expanded={drawerOpen}
					onclick={() => (drawerOpen = !drawerOpen)}
				>
					{#if drawerOpen}
						<XIcon size={20} weight="bold" />
					{:else}
						<ListIcon size={20} weight="bold" />
					{/if}
				</button>
			{/if}
			<a href="/" class="brand">amplio</a>
			{#if !viewport.isPhone}
				<nav class="nav">{@render navItems()}</nav>
			{/if}
		</div>
		<div class="right">
			{#if auth.ready && !auth.authed}
				<!-- Readonly (shared) view: the host status chips are the owner's
				     machine, not the viewer's concern — replace them with a banner. -->
				<span class="readonly" title="Read-only shared view. Sign-in actions are disabled.">
					<EyeIcon size={14} weight="bold" />
					Readonly view{auth.user ? ` of ${auth.user}'s runs` : ''}
				</span>
			{:else}
				<CpuChip />
				<MemChip />
				<SwapChip />
				<!-- Corp credential chip: only renders when the build supplies the
				     probe (INTERNAL) AND the probe has produced a value. Field
				     guard is defense in depth against a stale flag. -->
				{#if INTERNAL && sysstat.credentialSeconds !== null}
					<GcertChip />
				{/if}
			{/if}
		</div>
	</div>
	{#if viewport.isPhone && drawerOpen}
		<!-- Both live INSIDE the sticky bar so they hang off its bottom edge
		     whatever height it happens to be, and inherit its stacking context (no
		     second z-index scale to keep straight against the run views). -->
		<button class="scrim" aria-label="Close menu" onclick={() => (drawerOpen = false)}></button>
		<nav class="drawer" transition:fly={{ y: -8, duration: 150 }}>{@render navItems()}</nav>
	{/if}
</header>

<style>
	.bar {
		border-bottom: 1px solid var(--border);
		background: var(--bg-elev);
		position: sticky;
		top: 0;
		z-index: 10;
	}
	/* Mirror <main>'s max-width + horizontal padding so the brand/chips align
	   with the content column on wide displays. Keep these two in sync if you
	   change either: src/routes/+layout.svelte's `main` rule. */
	.inner {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 1rem;
		max-width: 1600px;
		margin: 0 auto;
		padding: 0.6rem clamp(1.2rem, 3vw, 2.5rem);
	}
	.left {
		display: flex;
		align-items: center;
		gap: 1.4rem;
		min-width: 0;
	}
	.right {
		display: flex;
		align-items: center;
		gap: 0.5rem;
	}
	.readonly {
		display: inline-flex;
		align-items: center;
		gap: 0.4rem;
		font-size: var(--fs-sm);
		color: var(--text-dim);
		border: 1px solid var(--border);
		border-radius: var(--radius-pill);
		padding: 0.2rem 0.7rem;
	}
	.brand {
		font-weight: 700;
		font-size: var(--fs-xl);
		color: var(--text);
		letter-spacing: 0.02em;
	}
	.brand:hover {
		text-decoration: none;
		color: var(--accent);
	}
	.nav {
		display: flex;
		gap: 1.1rem;
		font-size: var(--fs-md);
	}
	.nav-item {
		display: inline-flex;
		align-items: center;
		gap: 0.35rem;
		color: var(--text-dim);
	}
	.nav-item:hover {
		color: var(--accent);
		text-decoration: none;
	}
	/* Count suffix — color-coded so a glance distinguishes "moving" (active,
	   green) from "needs your eyes" (unread, gold). No border/pill: too noisy
	   next to the icon + label. Plain parens read as natural language. */
	.count {
		font-family: var(--mono);
		font-size: var(--fs-sm);
	}
	.count-active {
		color: var(--ok);
	}
	.count-unread {
		color: var(--accent);
	}
	/* Keep counter color stable when hovering the parent link, so they don't
	   flicker to accent on hover (the label color change already signals it). */
	.nav-item:hover .count-active {
		color: var(--ok);
	}
	.nav-item:hover .count-unread {
		color: var(--accent);
	}

	/* Phone additions. The desktop rules above are untouched; everything here is
	   either a phone-only element (.menu-toggle, .drawer, .scrim) or lives in the
	   media query below. */
	.menu-toggle {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		min-width: 44px;
		padding: 0 0.5rem;
		color: var(--text-dim);
		background: none;
		border: none;
	}
	.scrim {
		position: absolute;
		inset: 100% 0 auto 0;
		height: 100dvh;
		background: color-mix(in srgb, var(--bg) 65%, transparent);
		border: none;
		border-radius: 0;
		padding: 0;
	}
	.drawer {
		position: absolute;
		inset: 100% 0 auto 0;
		display: flex;
		flex-direction: column;
		background: var(--bg-elev);
		border-bottom: 1px solid var(--border);
		padding: 0.3rem 0;
	}
	.drawer .nav-item {
		/* Full-width rows, not the inline chips of the desktop nav: a drawer entry
		   is a tap target, so the whole line is hit area. */
		gap: 0.6rem;
		min-height: 44px;
		padding: 0 clamp(1.2rem, 3vw, 2.5rem);
		color: var(--text);
	}

	@media (max-width: 640px) {
		.inner {
			/* Tighter gutter than the desktop clamp, and a smaller gap: the status
			   chips need every pixel to stay on one line at 360px. */
			gap: 0.5rem;
			padding: 0.4rem 0.6rem;
		}
		.left {
			gap: 0.4rem;
			/* Rigid: squeezed to 100px against 113px of content, the wordmark spilled
			   into the first chip. The chips are the scrollable half, not this. */
			flex: 0 0 auto;
		}
		.brand {
			font-size: var(--fs-lg);
			flex-shrink: 0;
		}
		.right {
			gap: 0.3rem;
			flex: 1 1 0;
			justify-content: flex-end;
			min-width: 0;
			/* Chips are the one thing here that can legitimately outgrow the row
			   (four of them with the corp build). Scrolling them beats wrapping the
			   bar to two lines or truncating a number mid-digit. */
			overflow-x: auto;
			scrollbar-width: none;
		}
		/* ...which only scrolls if the chips refuse to be squeezed. */
		.right > :global(*) {
			flex-shrink: 0;
		}
	}
</style>
