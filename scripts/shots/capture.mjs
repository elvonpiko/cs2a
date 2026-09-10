// Captures screenshots of the real cs2a panel running against the demo
// agent (tools/cs2a-demo). Requires the demo panel to be up:
//
//   go run ./tools/cs2a-demo &
//   node scripts/shots/capture.mjs docs/img
//
// Produces: server.png, loadout.png, plugins.png, access.png (and a few
// supporting shots) at 1440x900, device scale 2 for crisp README rendering.
import { chromium } from 'playwright-core';
import { mkdirSync } from 'node:fs';

const out = process.argv[2] || 'docs/img';
const base = process.env.CS2A_DEMO_URL || 'http://127.0.0.1:8800';
mkdirSync(out, { recursive: true });

const browser = await chromium.launch();
const ctx = await browser.newContext({
	viewport: { width: 1440, height: 900 },
	deviceScaleFactor: 2,
	colorScheme: 'dark',
});
const page = await ctx.newPage();

// Sign in as the admin demo account.
await page.goto(base + '/login', { waitUntil: 'load' });
await page.fill('input[name=username]', 'admin');
await page.fill('input[name=password]', 'demo-password');
await page.click('button[type=submit]');
await page.waitForLoadState('networkidle');

// Toasts and confirm dialogs would pollute the shots; auto-dismiss.
page.on('dialog', (d) => d.accept());

async function shot(name, path, opts = {}) {
	// "load" not "networkidle": the panel's htmx pollers keep connections
	// open forever, so networkidle never fires on some pages.
	await page.goto(base + path, { waitUntil: 'load' });
	// htmx polls settle content (status card, log card); one settle beat is
	// enough for the server page, and images need a decode pass.
	await page.waitForTimeout(900);
	if (opts.scrollTo) await page.mouse.wheel(0, opts.scrollTo);
	if (opts.waitFor) await page.waitForSelector(opts.waitFor, { timeout: 5000 }).catch(() => {});
	await page.screenshot({ path: `${out}/${name}.png`, fullPage: !!opts.full });
	console.log('captured', name);
}

await shot('server', '/');
// The loadout picker is the most visual thing cs2a has. The page is far
// taller than a viewport, so instead of a 30k-pixel full-page capture,
// scroll to the weapons grid — real skin images in frame — and take a
// viewport shot of exactly that.
{
	await page.goto(base + '/loadout', { waitUntil: 'load' });
	await page.waitForTimeout(900);
	const weapons = page.locator('h2', { hasText: 'Weapons' }).first();
	await weapons.scrollIntoViewIfNeeded().catch(() => page.mouse.wheel(0, 1200));
	await page.waitForTimeout(400);
	await page.screenshot({ path: `${out}/loadout.png` });
	console.log('captured loadout');
}
await shot('plugins', '/plugins');
await shot('access', '/access');
await shot('users', '/users', { full: true });
await shot('settings', '/settings');

// A clean signed-out login shot: a fresh context has no session cookie.
const anon = await browser.newContext({
	viewport: { width: 1440, height: 900 },
	deviceScaleFactor: 2,
	colorScheme: 'dark',
});
const anonPage = await anon.newPage();
await anonPage.goto(base + '/login', { waitUntil: 'load' });
await anonPage.screenshot({ path: `${out}/login.png` });
console.log('captured login');
await anon.close();

await browser.close();
console.log('done ->', out);
