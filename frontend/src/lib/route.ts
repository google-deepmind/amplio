/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import type { Component } from 'svelte';
import type { IconComponentProps } from 'phosphor-svelte';
import { RowsIcon, PulseIcon, BellIcon, BookOpenIcon, InfoIcon } from 'phosphor-svelte';

/** Inside a run, whose shell brings its own bar and nav. */
export const isRunPath = (path: string) => path.startsWith('/runs/');
/** The dashboard. */
export const isHomePath = (path: string) => path === '/';

export type Destination = {
	label: string;
	href: string;
	icon: Component<IconComponentProps>;
	title?: string;
	count?: number;
	tone?: 'active' | 'unread';
};

/** The app-wide destinations: the desktop banner nav and both phone drawers. */
export function globalDestinations(activeCount: number, unreadCount: number): Destination[] {
	return [
		{ label: 'All Runs', href: '/', icon: RowsIcon },
		{
			label: 'Active Runs',
			href: '/?filter=active',
			icon: PulseIcon,
			title: `${activeCount} run(s) not yet terminal`,
			count: activeCount,
			tone: 'active'
		},
		{
			label: 'Recent Updates',
			href: '/?filter=updates',
			icon: BellIcon,
			title: `${unreadCount} run(s) with unseen updates`,
			count: unreadCount,
			tone: 'unread'
		},
		{ label: 'Recall', href: '/recall', icon: BookOpenIcon },
		{ label: 'About', href: '/about', icon: InfoIcon }
	];
}

/** Whether `url` is the page `href` leads to (the dashboard's ?filter included). */
export function isCurrent(href: string, url: URL): boolean {
	const to = new URL(href, url);
	if (to.pathname !== url.pathname) return false;
	return to.pathname !== '/' || to.searchParams.get('filter') === url.searchParams.get('filter');
}
