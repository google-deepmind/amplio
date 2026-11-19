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
	// Why a composer card didn't do what was asked (a refused shortcut, a send
	// while a form is open), as a small popover above the card, where its forms
	// open (OptionPopover), rather than a line below it. It goes with a press
	// outside it or Escape (onclose), and when the hint clears: the operator
	// types on, or does what it says.
	import { untrack } from 'svelte';
	import { WarningCircleIcon } from 'phosphor-svelte';
	import { placeAbove } from '$lib/anchoredPlace';

	let {
		band,
		text,
		onclose
	}: {
		band: HTMLElement | undefined; // the card
		text: string;
		onclose: () => void;
	} = $props();

	let pop = $state<HTMLElement>();

	$effect(() => {
		const el = pop;
		if (!el) return;
		// The top layer: above every stacking context of the page around it.
		if (typeof el.showPopover === 'function' && !el.matches(':popover-open')) el.showPopover();
		const place = () => {
			const b = band?.getBoundingClientRect();
			if (!b) return;
			const at = placeAbove(
				b.left,
				b,
				{ width: el.offsetWidth, height: el.offsetHeight },
				{ width: innerWidth, height: innerHeight }
			);
			el.style.left = `${at.left}px`;
			el.style.top = `${at.top}px`;
		};
		untrack(place); // its reads aren't this effect's: it re-runs on layout changes instead
		const ro = new ResizeObserver(place);
		ro.observe(el);
		if (band) ro.observe(band);
		const outside = (e: PointerEvent) => {
			if (!el.contains(e.target as Node)) onclose();
		};
		const onKey = (e: KeyboardEvent) => {
			if (e.key === 'Escape') onclose();
		};
		addEventListener('resize', place);
		addEventListener('scroll', place, true);
		document.addEventListener('pointerdown', outside, true);
		document.addEventListener('keydown', onKey);
		return () => {
			ro.disconnect();
			removeEventListener('resize', place);
			removeEventListener('scroll', place, true);
			document.removeEventListener('pointerdown', outside, true);
			document.removeEventListener('keydown', onKey);
		};
	});
</script>

<div bind:this={pop} class="notice small" popover="manual" role="status" aria-live="polite">
	<WarningCircleIcon size={16} weight="bold" />
	<span>{text}</span>
</div>

<style>
	/* As an option's form pops up (OptionPopover), smaller. */
	.notice {
		display: flex;
		align-items: flex-start;
		gap: 0.45rem;
		position: fixed;
		z-index: 40;
		inset: auto;
		margin: 0;
		width: max-content;
		max-width: min(30rem, calc(100vw - 16px));
		height: auto;
		padding: 0.55rem 0.75rem;
		color: var(--text);
		background: var(--bg-elev);
		border: 1px solid color-mix(in srgb, var(--text-dim) 30%, var(--border));
		border-radius: var(--radius-lg);
		box-shadow: 0 10px 32px rgb(0 0 0 / 0.5);
	}
	.notice :global(svg) {
		flex-shrink: 0;
		margin-top: 0.1rem;
		color: var(--warn);
	}
</style>
