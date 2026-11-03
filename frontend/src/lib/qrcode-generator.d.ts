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

// The package ships typings, but its "exports" map has no "types" condition,
// so "moduleResolution": "bundler" never finds them. Only what we use.
declare module 'qrcode-generator' {
	interface QRCode {
		addData(data: string): void;
		make(): void;
		getModuleCount(): number;
		isDark(row: number, col: number): boolean;
	}
	export default function qrcode(typeNumber: number, level: 'L' | 'M' | 'Q' | 'H'): QRCode;
}
