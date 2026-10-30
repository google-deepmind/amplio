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
	import { EyeIcon } from 'phosphor-svelte';
	import { page } from '$app/state';
	import { runsStore } from '$lib/runsStore.svelte';
	import { updates } from '$lib/updates.svelte';
	import { auth } from '$lib/auth.svelte';
	import { sysstat } from '$lib/sysstat.svelte';
	import { viewport } from '$lib/viewport.svelte';
	import { globalDestinations, isRunPath } from '$lib/route';
	import { GcertChip, INTERNAL } from './internal';
	import CpuChip from './CpuChip.svelte';
	import MemChip from './MemChip.svelte';
	import SwapChip from './SwapChip.svelte';
	import Drawer from './Drawer.svelte';
	import MenuButton from './MenuButton.svelte';

	// Global, app-wide banner. Holds branding + global navigation on the left
	// (three discoverable filter targets), and the global status cluster on
	// the right (cpu/mem/swap/gcert). Run-scoped info belongs in the run
	// header below it, not here — keeps this strip stable across navigation.
	//
	// Nav items are stable (always present) so layout never shifts as runs
	// come and go; only the (N) suffix appears/disappears with the count.
	// On a phone the row holds the brand and the chips; the nav is in the drawer.
	const destinations = $derived(globalDestinations(runsStore.activeCount, updates.count, auth.ready && !auth.authed));
	// Inside a run on a phone the run shell shows its own compact bar (menu,
	// title, status) and drawer, so this bar and its drawer stay out.
	const inRun = $derived(isRunPath(page.url.pathname));
</script>

<header class="bar" class:hide-on-phone={inRun}>
	<!-- Edge-to-edge background (the .bar) with the actual contents constrained
	     to the same column as <main> below (the .inner). On wide displays the
	     brand + status chips stay aligned with the content column instead of
	     drifting to the screen corners; on narrower screens both layers
	     collapse to the same width so the seam is invisible. -->
	<!-- The entries come from $lib/route, which the phone drawer reads too. -->
	{#snippet navItems()}
		{#each destinations as d (d.href)}
			<a href={d.href} class="nav-item" title={d.title}>
				<d.icon size={14} weight="bold" />
				<span>{d.label}</span>
				{#if d.count}
					<span class="count count-{d.tone}">({d.count})</span>
				{/if}
			</a>
		{/each}
	{/snippet}
	<div class="inner">
		<div class="left">
			{#if viewport.isPhone && !inRun}<MenuButton />{/if}
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
</header>
{#if viewport.isPhone && !inRun}<Drawer />{/if}

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

	@media (max-width: 640px) {
		.bar {
			height: var(--bar-h);
		}
		.bar.hide-on-phone {
			display: none;
		}
		/* ☰ and title land where the run bar puts them (spec §2.2). */
		.inner {
			height: 100%;
			gap: var(--sp-2);
			padding: 0 var(--sp-2) 0 var(--sp-1);
		}
		.left {
			gap: var(--sp-1);
			/* Rigid: squeezed to 100px against 113px of content, the wordmark spilled
			   into the first chip. The chips are the scrollable half, not this. */
			flex: 0 0 auto;
		}
		.brand {
			display: inline-flex;
			align-items: center;
			height: var(--tap);
			font-size: var(--fs-lg);
			font-weight: 600;
			flex-shrink: 0;
		}
		.right {
			gap: var(--sp-1);
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
