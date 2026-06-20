/**
 * E2e tests for the transit examples/spa filter using Playwright + Chrome.
 *
 * Prerequisites:
 *   - Envoy running with the spa .so loaded (see run.sh)
 *   - npm install
 *   - npx playwright install chrome
 *
 * Run:
 *   node --test spa.test.mjs
 *   SPA_URL=http://localhost:10000 node --test spa.test.mjs
 *
 * Set PLAYWRIGHT_CHANNEL=chromium to use Playwright's bundled Chromium instead
 * of the default Google Chrome stable channel.
 */

import assert from "node:assert/strict";
import { describe, it, before, after } from "node:test";
import { chromium } from "playwright";

const BASE = process.env.SPA_URL ?? "http://localhost:10000";
const CHANNEL = process.env.PLAYWRIGHT_CHANNEL ?? "chrome";

let browser;
let context;

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

before(async () => {
  browser = await chromium.launch({
    channel: CHANNEL,
    headless: true,
  });
  context = await browser.newContext();
});

after(async () => {
  await context?.close();
  await browser?.close();
});

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

async function withPage(path, fn) {
  const page = await context.newPage();
  await page.goto(`${BASE}${path}`, { waitUntil: "domcontentloaded" });
  try {
    await fn(page);
  } finally {
    await page.close();
  }
}

// ---------------------------------------------------------------------------
// Home page
// ---------------------------------------------------------------------------

describe("Home page (/)", () => {
  it("renders the React root #root element", () =>
    withPage("/", async (page) => {
      const root = await page.$("#root");
      assert.ok(root, "#root must exist");
    }));

  it("shows the Home h2 heading", () =>
    withPage("/", async (page) => {
      const text = await page.$eval("h2", (el) => el.textContent.trim());
      assert.equal(text, "Home");
    }));

  it("nav bar has three links: Home, About, Dashboard", () =>
    withPage("/", async (page) => {
      const links = await page.$$eval("nav a", (els) =>
        els.map((a) => a.textContent.trim())
      );
      assert.deepEqual(links, ["Home", "About", "Dashboard"]);
    }));
});

// ---------------------------------------------------------------------------
// About page (client-side route — filter must serve index.html on hard refresh)
// ---------------------------------------------------------------------------

describe("About page (/about)", () => {
  it("renders About heading on direct navigation", () =>
    withPage("/about", async (page) => {
      const text = await page.$eval("h2", (el) => el.textContent.trim());
      assert.equal(text, "About");
    }));
});

// ---------------------------------------------------------------------------
// Dashboard page + /api/time integration
// ---------------------------------------------------------------------------

describe("Dashboard page (/dashboard)", () => {
  it("renders the Dashboard h2 heading", () =>
    withPage("/dashboard", async (page) => {
      const text = await page.$eval("h2", (el) => el.textContent.trim());
      assert.equal(text, "Dashboard");
    }));

  it("Fetch server time button returns ISO timestamp from the .so", () =>
    withPage("/dashboard", async (page) => {
      await page.click("button");
      await page.waitForFunction(
        () => document.body.innerText.includes("api-backend"),
        { timeout: 5000 }
      );
      const bodyText = await page.evaluate(() => document.body.innerText);
      assert.match(bodyText, /api-backend/,        '"api-backend" must appear in page');
      assert.match(bodyText, /\d{4}-\d{2}-\d{2}T/, "ISO timestamp must appear in page");
    }));
});

// ---------------------------------------------------------------------------
// SPA fallback — unknown routes must return index.html so React Router works
// ---------------------------------------------------------------------------

describe("SPA fallback routing", () => {
  for (const path of ["/unknown-page", "/deep/nested/route"]) {
    it(`${path} — spa filter returns index.html, #root is present`, () =>
      withPage(path, async (page) => {
        const root = await page.$("#root");
        assert.ok(root, `#root must exist on ${path}`);
      }));
  }
});

// ---------------------------------------------------------------------------
// API endpoints — exercised via in-page fetch (same origin, no CORS)
// ---------------------------------------------------------------------------

describe("GET /api/hello", () => {
  it("returns JSON with message from inside the .so", () =>
    withPage("/", async (page) => {
      const data = await page.evaluate(async (base) => {
        const res = await fetch(`${base}/api/hello`);
        return res.json();
      }, BASE);

      assert.equal(data.message, "hello from inside the .so");
      assert.equal(data.filter,  "api-backend");
    }));
});

describe("GET /api/time", () => {
  it("returns a valid ISO 8601 UTC timestamp", () =>
    withPage("/", async (page) => {
      const data = await page.evaluate(async (base) => {
        const res = await fetch(`${base}/api/time`);
        return res.json();
      }, BASE);

      const ts = new Date(data.time);
      assert.ok(!isNaN(ts.getTime()), `time must parse as a date, got: ${data.time}`);
    }));
});

describe("GET /api/unknown", () => {
  it("returns 404 with error JSON from the .so", () =>
    withPage("/", async (page) => {
      const [status, body] = await page.evaluate(async (base) => {
        const res = await fetch(`${base}/api/unknown`);
        return [res.status, await res.json()];
      }, BASE);

      assert.equal(status,     404);
      assert.equal(body.error, "not found");
    }));
});

// ---------------------------------------------------------------------------
// Static asset cache headers
// ---------------------------------------------------------------------------

describe("Static assets", () => {
  it("fingerprinted JS under /assets/ has immutable cache header", () =>
    withPage("/", async (page) => {
      const scriptSrc = await page.$eval("script[src]", (el) => el.getAttribute("src"));
      assert.match(scriptSrc, /^\/assets\//, "script src must be under /assets/");

      const cacheControl = await page.evaluate(async ({ base, src }) => {
        const res = await fetch(`${base}${src}`);
        return res.headers.get("cache-control");
      }, { base: BASE, src: scriptSrc });

      assert.match(cacheControl, /immutable/, "fingerprinted assets must be immutable");
    }));

  it("index.html has no-cache header", () =>
    withPage("/", async (page) => {
      const cacheControl = await page.evaluate(async (base) => {
        const res = await fetch(`${base}/`);
        return res.headers.get("cache-control");
      }, BASE);

      assert.equal(cacheControl, "no-cache");
    }));
});
