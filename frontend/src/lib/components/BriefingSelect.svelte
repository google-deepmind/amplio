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
	import { onMount } from 'svelte';
	import type { BriefingInfo } from '$lib/types';
	import { cachedBriefings, loadBriefings } from '$lib/briefingLibrary';
	import { NotebookIcon, CaretDownIcon } from 'phosphor-svelte';

	// Briefings are opt-in per run: an empty selection means none, so there is no
	// default set to reconcile with and nothing to latch.
	let {
		selected = $bindable<string[]>([]),
		ondirty
	}: { selected?: string[]; ondirty?: () => void } = $props();

	// Seed synchronously from the prefetched cache (the common case) so the
	// control is there when the composer expands, rather than popping in a beat
	// later and shifting the row.
	let all = $state<BriefingInfo[]>(cachedBriefings() ?? []);
	let open = $state(false);
	let error = $state('');

	onMount(async () => {
		if (all.length > 0) return; // cache hit: nothing to wait for
		try {
			all = await loadBriefings();
		} catch (e) {
			error = String(e);
		}
	});

	// keepFocus stops a row from TAKING focus on mousedown (the click still
	// fires). Without it the browser parks focus on <body>, the form's focusout
	// sees a null relatedTarget, reads it as "focus left the form", and collapses
	// the whole composer — menu and all — on the FIRST click, before anything has
	// marked the form dirty. ModelSelect carries the same guard for the same
	// reason.
	function keepFocus(e: MouseEvent) {
		e.preventDefault();
	}

	function toggle(name: string) {
		ondirty?.(); // choosing a briefing IS editing the form: keep it expanded
		selected = selected.includes(name)
			? selected.filter((n) => n !== name)
			: [...selected, name];
	}

	function clickOutside(node: HTMLElement, cb: () => void) {
		function handle(e: MouseEvent) {
			if (!node.contains(e.target as Node)) cb();
		}
		document.addEventListener('click', handle, true);
		return {
			destroy() {
				document.removeEventListener('click', handle, true);
			}
		};
	}

	// Group by the part of the name before the last "/", which is how a library
	// organizes itself (workflow/…, corp/…) without needing a separate field.
	const groups = $derived.by(() => {
		const out = new Map<string, BriefingInfo[]>();
		for (const b of all) {
			const i = b.name.lastIndexOf('/');
			const g = i > 0 ? b.name.slice(0, i) : '';
			out.set(g, [...(out.get(g) ?? []), b]);
		}
		return [...out.entries()].sort(([a], [b]) => a.localeCompare(b));
	});
	const leaf = (name: string) => name.slice(name.lastIndexOf('/') + 1);
</script>

<!-- Nothing to choose from: don't spend a control on it. -->
{#if all.length > 0}
	<div class="wrap" use:clickOutside={() => (open = false)}>
		<button
			type="button"
			class="trigger"
			title="Briefings — extra context added to this run's prompt"
			onclick={() => (open = !open)}
		>
			<NotebookIcon size={16} />
			<span class="label">
				{selected.length > 0 ? `briefings · ${selected.length}` : 'briefings'}
			</span>
			<span class="caret"><CaretDownIcon size={12} /></span>
		</button>
		{#if open}
			<div class="menu">
				{#each groups as [group, items] (group)}
					{#if group}<div class="group">{group}</div>{/if}
					{#each items as b (b.name)}
						<!-- A button, not a <label><input>: the row needs a mousedown guard
						     (see keepFocus) and only an interactive element may carry one. -->
						<button
							type="button"
							class="row"
							role="checkbox"
							aria-checked={selected.includes(b.name)}
							onmousedown={keepFocus}
							onclick={() => toggle(b.name)}
						>
							<span class="box" aria-hidden="true">{selected.includes(b.name) ? '✓' : ''}</span>
							<span class="text">
								<span class="name">
									{leaf(b.name)}
									{#if b.scope === 'root'}<span class="tag">root only</span>{/if}
									{#if b.source === 'user'}<span class="tag">yours</span>{/if}
								</span>
								<span class="desc">{b.description}</span>
							</span>
						</button>
					{/each}
				{/each}
				{#if error}<div class="error">{error}</div>{/if}
			</div>
		{/if}
	</div>
{/if}

<style>
	.wrap {
		position: relative;
	}
	.trigger {
		display: inline-flex;
		align-items: center;
		gap: 0.35rem;
		background: var(--bg-elev);
		border: 1px solid var(--border);
		color: var(--text);
		border-radius: var(--radius-sm);
		padding: 0.35rem 0.6rem;
		cursor: pointer;
		font: inherit;
	}
	.caret {
		display: inline-flex;
		color: var(--text-dim);
	}
	.menu {
		position: absolute;
		bottom: calc(100% + 4px);
		left: 0;
		min-width: 320px;
		max-width: 30rem;
		max-height: 65vh;
		overflow: auto;
		background: var(--bg-elev);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		box-shadow: var(--shadow, 0 6px 24px rgb(0 0 0 / 25%));
		padding: 0.25rem;
		z-index: 20;
	}
	.group {
		padding: 0.35rem 0.5rem 0.15rem;
		color: var(--text-dim);
		font-size: 0.75rem;
		text-transform: uppercase;
		letter-spacing: 0.04em;
	}
	.row {
		width: 100%;
		background: none;
		border: 0;
		color: inherit;
		font: inherit;
		text-align: left;
		display: flex;
		gap: 0.5rem;
		align-items: flex-start;
		padding: 0.4rem 0.5rem;
		border-radius: var(--radius-sm);
		cursor: pointer;
	}
	.row:hover {
		background: var(--bg-hover, rgb(255 255 255 / 6%));
	}
	.box {
		flex: none;
		width: 1rem;
		height: 1rem;
		margin-top: 0.15rem;
		border: 1px solid var(--border);
		border-radius: 3px;
		display: inline-flex;
		align-items: center;
		justify-content: center;
		font-size: 0.7rem;
		line-height: 1;
	}
	.text {
		display: flex;
		flex-direction: column;
		gap: 0.1rem;
	}
	.name {
		display: inline-flex;
		align-items: center;
		gap: 0.35rem;
	}
	.tag {
		font-size: 0.7rem;
		color: var(--text-dim);
		border: 1px solid var(--border);
		border-radius: 999px;
		padding: 0 0.35rem;
	}
	.desc {
		color: var(--text-dim);
		font-size: 0.8rem;
	}
	.error {
		color: var(--danger, #f66);
		padding: 0.4rem 0.5rem;
		font-size: 0.8rem;
	}
</style>
