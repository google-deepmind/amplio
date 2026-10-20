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
	// ☰ for the phone drawer: one component, so both bars put it in the same
	// place with the same look and the same ARIA.
	import { ListIcon } from 'phosphor-svelte';
	import { DRAWER_ID, navDrawer } from '$lib/navDrawer.svelte';
</script>

<button
	class="menu-btn"
	aria-label="Menu"
	aria-haspopup="dialog"
	aria-controls={DRAWER_ID}
	aria-expanded={navDrawer.open}
	onclick={() => navDrawer.show()}
>
	<ListIcon size={20} weight="bold" />
</button>

<style>
	@media (max-width: 640px) {
		.menu-btn {
			display: inline-flex;
			align-items: center;
			justify-content: center;
			flex-shrink: 0;
			width: var(--tap);
			height: var(--tap);
			min-height: 0;
			padding: 0;
			background: none;
			border: none;
			color: var(--text);
		}
	}
</style>
