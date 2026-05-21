/**
 * E2e tests for the request-ui filter.
 *
 * Prerequisites (handled by run.sh):
 *   - Envoy running with librequest-ui.so loaded, proxying to testserver
 *   - request-ui HTTP server up (at UI_URL, default http://127.0.0.1:6062)
 *
 * Run:
 *   PROXY_URL=http://127.0.0.1:10000 UI_URL=http://127.0.0.1:6062 \
 *     node --test requi.test.mjs
 */

import assert from "node:assert/strict";
import { describe, it, before } from "node:test";

const PROXY = process.env.PROXY_URL ?? "http://127.0.0.1:10000";
const UI    = process.env.UI_URL    ?? "http://127.0.0.1:6062";

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// poll calls fn() every intervalMs until it returns a truthy value or
// timeoutMs is exceeded. Returns the last truthy result.
async function poll(fn, { timeoutMs = 5000, intervalMs = 200 } = {}) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const result = await fn();
    if (result) return result;
    await new Promise((r) => setTimeout(r, intervalMs));
  }
  throw new Error(`poll timed out after ${timeoutMs}ms`);
}

// fetchRecords returns all records from the UI API, optionally filtered.
async function fetchRecords(params = {}) {
  const qs = new URLSearchParams(params).toString();
  const url = `${UI}/api/requests${qs ? "?" + qs : ""}`;
  const resp = await fetch(url);
  assert.equal(resp.status, 200, `GET ${url} status`);
  return await resp.json();
}

// readSseEvents reads SSE events from /api/stream until count events are
// collected or the timeout fires. Returns the collected events.
async function readSseEvents(count, timeoutMs = 5000) {
  const controller = new AbortController();
  const deadline = setTimeout(() => controller.abort(), timeoutMs);
  const events = [];

  try {
    const resp = await fetch(`${UI}/api/stream`, { signal: controller.signal });
    const reader = resp.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";

    outer: while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      const lines = buf.split("\n");
      buf = lines.pop() ?? "";
      for (const line of lines) {
        if (line.startsWith("data: ")) {
          try {
            events.push(JSON.parse(line.slice(6)));
          } catch (_) {}
          if (events.length >= count) break outer;
        }
      }
    }
  } catch (err) {
    if (err.name !== "AbortError") throw err;
  } finally {
    clearTimeout(deadline);
  }

  return events;
}

// ---------------------------------------------------------------------------
// Test state
// ---------------------------------------------------------------------------

// Unique path prefix so each test run's records are identifiable.
const RUN_ID = `run-${Date.now()}`;

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe("request-ui e2e", () => {
  before(async () => {
    // Warm up: ensure at least one request has been proxied so the access
    // logger has fired and the UI HTTP server is accepting connections.
    await fetch(`${PROXY}/health`).catch(() => {});
    await new Promise((r) => setTimeout(r, 300));
  });

  it("UI serves HTML", async () => {
    const resp = await fetch(`${UI}/`);
    assert.equal(resp.status, 200);
    const ct = resp.headers.get("content-type") ?? "";
    assert.ok(ct.includes("text/html"), `expected text/html, got ${ct}`);
    const body = await resp.text();
    assert.ok(body.includes("<html"), "page should contain <html");
  });

  it("/api/requests returns JSON array", async () => {
    const records = await fetchRecords();
    assert.ok(Array.isArray(records), "should return an array");
  });

  it("proxied GET request appears in records", async () => {
    const path = `/${RUN_ID}/api/hello`;
    const upstream = await fetch(`${PROXY}${path}`);
    assert.equal(upstream.status, 200);

    const found = await poll(async () => {
      const recs = await fetchRecords({ q: RUN_ID });
      return recs.find((r) => r.path === path && r.method === "GET");
    });

    assert.ok(found, "record not found for GET request");
    assert.equal(found.method, "GET");
    assert.equal(found.path, path);
    assert.equal(found.response_code, 200);
    assert.ok(found.request_id, "request_id should be populated");
    assert.ok(found.duration_ms >= 0, "duration_ms should be non-negative");
  });

  it("proxied POST request appears with correct method", async () => {
    const path = `/${RUN_ID}/api/create`;
    await fetch(`${PROXY}${path}`, { method: "POST", body: '{"x":1}' });

    const found = await poll(async () => {
      const recs = await fetchRecords({ q: RUN_ID });
      return recs.find((r) => r.path === path && r.method === "POST");
    });

    assert.ok(found, "record not found for POST request");
    assert.equal(found.method, "POST");
    // testserver returns 201 for /api/create
    assert.equal(found.response_code, 201);
  });

  it("5xx response is flagged as has_error", async () => {
    const path = `/${RUN_ID}/api/error`;
    await fetch(`${PROXY}${path}`);

    const found = await poll(async () => {
      const recs = await fetchRecords({ q: RUN_ID });
      return recs.find((r) => r.path === path && r.response_code >= 500);
    });

    assert.ok(found, "500 record not found");
    assert.equal(found.has_error, true, "has_error should be true for 5xx");
  });

  it("?errors=1 filter returns only error records", async () => {
    // Ensure at least one error record exists from prior tests.
    const errorRecs = await fetchRecords({ errors: "1" });
    for (const r of errorRecs) {
      assert.equal(r.has_error, true, `non-error record returned: ${JSON.stringify(r)}`);
    }
  });

  it("?q= search filters by path", async () => {
    const recs = await fetchRecords({ q: RUN_ID });
    assert.ok(recs.length > 0, "search should return records for this run");
    for (const r of recs) {
      assert.ok(
        r.path?.includes(RUN_ID) ||
        r.request_id?.includes(RUN_ID) ||
        r.method?.toLowerCase().includes(RUN_ID),
        `record doesn't match search: ${JSON.stringify(r)}`
      );
    }
  });

  it("SSE stream delivers new records in real-time", async () => {
    const path = `/${RUN_ID}/sse-probe`;

    // Start listening before sending the request.
    const eventsPromise = readSseEvents(1, 6000);
    await new Promise((r) => setTimeout(r, 50));
    await fetch(`${PROXY}${path}`);

    const events = await eventsPromise;
    assert.ok(events.length >= 1, "expected at least 1 SSE event");

    // The SSE event is the full record JSON.
    const rec = events[0];
    assert.ok(typeof rec === "object" && rec !== null, "SSE event should be a JSON object");
    assert.ok(rec.id > 0, "SSE record should have an id");
    assert.ok(rec.request_id, "SSE record should have request_id");
  });

  it("response_headers are recorded", async () => {
    const path = `/${RUN_ID}/headers-check`;
    await fetch(`${PROXY}${path}`);

    const found = await poll(async () => {
      const recs = await fetchRecords({ q: RUN_ID });
      return recs.find((r) => r.path === path);
    });

    assert.ok(found, "record not found");
    assert.ok(found.response_headers, "response_headers should be recorded");
    const headers = JSON.parse(found.response_headers);
    assert.ok(Array.isArray(headers), "response_headers should be a JSON array");
    const statusHeader = headers.find(([k]) => k === ":status");
    assert.ok(statusHeader, "should include :status pseudo-header");
  });

  it("?since= returns only records after the given id", async () => {
    // Get current max id.
    const before = await fetchRecords({ limit: "1" });
    const sinceId = before.length > 0 ? before[0].id : 0;

    // Send a new request.
    const path = `/${RUN_ID}/since-probe`;
    await fetch(`${PROXY}${path}`);
    await poll(async () => {
      const recs = await fetchRecords({ q: RUN_ID });
      return recs.find((r) => r.path === path);
    });

    const newRecs = await fetchRecords({ since: String(sinceId) });
    assert.ok(newRecs.length >= 1, "should return at least the new record");
    for (const r of newRecs) {
      assert.ok(r.id > sinceId, `record id ${r.id} should be > ${sinceId}`);
    }
  });
});
