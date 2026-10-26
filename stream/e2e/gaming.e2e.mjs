// Browser end-to-end test for MEA Stream Gaming Mode.
//
// Runs the real mea-web Gaming Mode page (built dist), the real gateway and
// the real agent capturing an Xvfb desktop with ffmpeg/x264, in Chromium.
// The MEA API is mocked with Playwright routing; tokens are minted with the
// real Ed25519 key through mea-stream-token. Verifies:
//   - WebRTC video and audio play in the browser
//   - mouse and keyboard reach the X server (checked with xprobe)
//   - the stats overlay reports live numbers
//   - Disconnect releases held keys and frees the gateway slot
//   - a replayed or foreign-desktop token is refused
//
//   go build -o $BIN/ ./cmd/... ./e2e/xprobe
//   (cd mea-web/newbie && npm run build)
//   BIN=$BIN MEA_WEB_DIST=mea-web/newbie/dist node e2e/gaming.e2e.mjs
import { spawn, execFileSync } from "node:child_process";
import { createServer } from "node:http";
import { readFileSync, existsSync, writeFileSync, mkdtempSync } from "node:fs";
import { join, extname } from "node:path";
import { tmpdir } from "node:os";
import { createRequire } from "node:module";

// require() so a globally installed Playwright (NODE_PATH) also resolves.
const { chromium } = createRequire(import.meta.url)("playwright");

const BIN = process.env.BIN ?? "bin";
const DIST = process.env.MEA_WEB_DIST ?? "../../mea-web/newbie/dist";
const DISPLAY = process.env.E2E_DISPLAY ?? ":78";
const WEB = 5199, SIGNAL = 47443, ADMIN = 47991, MEDIA = 47999, AGENT = 47101;
const ORIGIN = `http://127.0.0.1:${WEB}`;
const ADMIN_TOKEN = "e2e-admin-token-0123456789";
const DESKTOP = "desk-e2e", HOST = "host-e2e";

const procs = [];
const work = mkdtempSync(join(tmpdir(), "mea-e2e-"));
let failures = 0;
const check = (ok, what, extra = "") => {
  console.log(`${ok ? "PASS" : "FAIL"} ${what}${extra ? " — " + extra : ""}`);
  if (!ok) failures++;
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function run(name, cmd, args, env) {
  const p = spawn(cmd, args, { env: { ...process.env, ...env }, stdio: ["ignore", "pipe", "pipe"] });
  const log = [];
  p.stdout.on("data", (d) => log.push(d.toString()));
  p.stderr.on("data", (d) => log.push(d.toString()));
  p.on("exit", (c) => code(name, c));
  procs.push({ name, p, log });
  return p;
}
let stopping = false;
function code(name, c) {
  if (!stopping) console.log(`${name} exited ${c}`);
}
async function waitFor(f, ms, what) {
  const end = Date.now() + ms;
  for (;;) {
    try {
      const v = await f();
      if (v) return v;
    } catch {}
    if (Date.now() > end) throw new Error("timeout: " + what);
    await sleep(100);
  }
}
const probe = () => JSON.parse(execFileSync(join(BIN, "xprobe"), { env: { ...process.env, DISPLAY } }).toString());
const metrics = async () =>
  (await fetch(`http://127.0.0.1:${ADMIN}/v1/metrics`, { headers: { Authorization: "Bearer " + ADMIN_TOKEN } })).json();

function keys() {
  const out = execFileSync(join(BIN, "mea-stream-token"), ["keygen"]).toString();
  return Object.fromEntries(out.trim().split("\n").map((l) => l.split(/=(.*)/s).slice(0, 2)));
}
function mint(priv, desktop = DESKTOP) {
  return execFileSync(join(BIN, "mea-stream-token"), ["mint", "-desktop", desktop, "-host", HOST, "-profile", "high"], {
    env: { ...process.env, MEA_STREAM_PRIVATE_KEY: priv },
  }).toString().trim();
}

function staticServer() {
  const types = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml" };
  const s = createServer((req, res) => {
    const path = new URL(req.url, ORIGIN).pathname;
    let file = join(DIST, path);
    if (!existsSync(file) || path === "/") file = join(DIST, "index.html");
    try {
      const body = readFileSync(file);
      res.writeHead(200, { "Content-Type": types[extname(file)] ?? "application/octet-stream" });
      res.end(body);
    } catch {
      res.writeHead(404).end();
    }
  });
  return new Promise((r) => s.listen(WEB, "127.0.0.1", () => r(s)));
}

async function main() {
  const k = keys();
  const registry = join(work, "agents.json");
  writeFileSync(registry, JSON.stringify({ desktops: { [DESKTOP]: `127.0.0.1:${AGENT}` } }));

  run("Xvfb", "Xvfb", [DISPLAY, "-screen", "0", "1280x720x24", "-nolisten", "tcp"]);
  await waitFor(() => probe(), 5000, "Xvfb");
  run("agent", join(BIN, "mea-stream-agent"), [], {
    DISPLAY, MEA_STREAM_PUBLIC_KEY: k.MEA_STREAM_PUBLIC_KEY, MEA_DESKTOP_ID: DESKTOP,
    MEA_ENCODER: "x264", MEA_AUDIO_TEST_PATTERN: "1", MEA_AGENT_LISTEN: `127.0.0.1:${AGENT}`,
  });
  run("gateway", join(BIN, "mea-stream-gateway"), [], {
    MEA_HOST_ID: HOST, MEA_STREAM_PUBLIC_KEY: k.MEA_STREAM_PUBLIC_KEY, MEA_MEDIA_PORT: String(MEDIA),
    MEA_ALLOWED_ORIGINS: ORIGIN, MEA_ADMIN_TOKEN: ADMIN_TOKEN, MEA_REGISTRY_FILE: registry,
    MEA_LISTEN: `127.0.0.1:${SIGNAL}`, MEA_ADMIN_LISTEN: `127.0.0.1:${ADMIN}`, MEA_MAX_SESSIONS: "1",
  });
  await waitFor(async () => (await fetch(`http://127.0.0.1:${SIGNAL}/healthz`)).ok, 5000, "gateway");
  const web = await staticServer();

  const api = { stream: 0, activity: [], disconnect: 0, lastToken: "" };
  const browser = await chromium.launch({ args: ["--autoplay-policy=no-user-gesture-required"] });
  const page = await (await browser.newContext({ viewport: { width: 1280, height: 860 } })).newPage();
  await page.addInitScript(() => {
    const PC = window.RTCPeerConnection;
    window.RTCPeerConnection = function (...a) {
      const pc = new PC(...a);
      window.__meaPC = pc;
      return pc;
    };
    window.RTCPeerConnection.prototype = PC.prototype;
  });
  page.on("response", (r) => r.status() >= 400 && console.log("browser: HTTP", r.status(), r.url()));
  page.on("console", (m) => m.type() === "error" && console.log("browser:", m.text()));
  const me = {
    user: { id: "user-e2e", username: "erin", email: "erin@example.test", name: "Erin", verified: true, subscription: "eleet" },
    csrf: "csrf", idleSeconds: 1200, warningSeconds: 1080, graceSeconds: 300,
  };
  const desk = { id: DESKTOP, state: "ready", operationId: "op", lastActivity: new Date().toISOString(), expiresAt: new Date(Date.now() + 3600e3).toISOString() };
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    const json = (b, status = 200) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify(b) });
    if (url.pathname === "/api/me") return json(me);
    if (url.pathname === "/api/desktops") return json({ desktop: desk });
    if (url.pathname.endsWith("/stream")) {
      api.stream++;
      api.lastToken = mint(k.MEA_STREAM_PRIVATE_KEY);
      return json({ token: api.lastToken, sessionId: "s", gatewayUrl: `http://127.0.0.1:${SIGNAL}`, iceServers: [], profile: "high", expiresIn: 60 });
    }
    if (url.pathname.endsWith("/activity")) {
      api.activity.push(route.request().postDataJSON());
      return json({ ok: true });
    }
    if (url.pathname.endsWith("/disconnect")) {
      api.disconnect++;
      return json({ ok: true });
    }
    return json({ error: "not mocked" }, 404);
  });

  await page.goto(ORIGIN + "/desktop/gaming");
  const indicator = page.locator(".connection-indicator");
  await indicator.getByText("Streaming").waitFor({ timeout: 20000 });
  check(true, "Gaming Mode reached Streaming");

  const v = await waitFor(
    () => page.evaluate(() => {
      const el = document.querySelector("video");
      return el && el.videoWidth > 0 && !el.paused && { w: el.videoWidth, h: el.videoHeight };
    }),
    10000, "video frames",
  );
  check(v.w === 1280 && v.h === 720, "video decodes at the desktop size", `${v.w}x${v.h}`);

  const rtp = await page.evaluate(async () => {
    const pc = window.__meaPC;
    if (!pc) return null;
    const out = {};
    (await pc.getStats()).forEach((s) => {
      if (s.type === "inbound-rtp") out[s.kind] = { packets: s.packetsReceived, frames: s.framesDecoded, codec: s.codecId };
    });
    return out;
  });
  if (rtp) {
    check(rtp.audio?.packets > 0, "Opus audio RTP arrives", JSON.stringify(rtp.audio));
    check(rtp.video?.frames > 0, "video frames decoded", JSON.stringify(rtp.video));
  }

  let m;
  // Absolute mouse: the desktop pointer follows the browser pointer.
  const box = await page.locator(".gaming-surface video").boundingBox();
  // the picture is letterboxed inside the element (object-fit: contain)
  const s = Math.min(box.width / v.w, box.height / v.h);
  const cx = box.x + (box.width - v.w * s) / 2, cy = box.y + (box.height - v.h * s) / 2;
  await page.mouse.move(cx + v.w * s * 0.25, cy + v.h * s * 0.75);
  const p = await waitFor(() => { const q = probe(); return Math.abs(q.x - 320) < 8 && Math.abs(q.y - 540) < 8 && q; }, 3000, "pointer").catch(() => probe());
  check(Math.abs(p.x - 320) < 8 && Math.abs(p.y - 540) < 8, "absolute mouse motion reaches X", `${p.x},${p.y}`);

  await page.mouse.down({ button: "left" });
  const b = await waitFor(() => probe().buttons & 1, 3000, "button").catch(() => 0);
  check(!!b, "mouse button reaches X");
  await page.mouse.up({ button: "left" });

  // Keyboard by physical position: KeyA → keysym 'a' (0x61).
  await page.locator(".gaming-surface").focus();
  await page.keyboard.down("KeyA");
  const held = await waitFor(() => probe().keys.includes(0x61), 3000, "key down").catch(() => false);
  check(held, "key down reaches X");
  await page.keyboard.up("KeyA");
  const up = await waitFor(() => !probe().keys.includes(0x61), 3000, "key up").catch(() => false);
  check(up, "key up reaches X");

  // Stats overlay shows live numbers.
  await page.getByTitle("Stream statistics").click();
  await sleep(2500);
  const overlay = await page.locator(".stats-overlay").innerText();
  check(/1280×720/.test(overlay) && /Mbps/.test(overlay), "stats overlay is live", overlay.replace(/\s+/g, " "));

  // Motion: the picture streams at a real frame rate and bitrate.
  run("paint", join(BIN, "xprobe"), ["paint"], { DISPLAY });
  await sleep(4000);
  m = await metrics();
  const moving = m.sessions?.[0] ?? {};
  check(moving.fps >= 25 && moving.kbps > 300, "moving desktop streams at frame rate", `${moving.fps?.toFixed?.(1)} fps, ${moving.kbps?.toFixed?.(0)} kbps, encoder ${moving.encoder}`);
  const decoded = await page.evaluate(async () => {
    let f = 0;
    (await window.__meaPC.getStats()).forEach((s) => { if (s.type === "inbound-rtp" && s.kind === "video") f = s.framesDecoded; });
    return f;
  });
  check(decoded > 60, "browser decodes the moving stream", `${decoded} frames`);
  procs.find((p) => p.name === "paint")?.p.kill("SIGTERM");

  // Profile change is applied live, without reconnecting.
  const before = api.stream;
  await page.getByLabel("Streaming quality").selectOption("economy");
  await sleep(1500);
  check(api.stream === before && (await indicator.innerText()).includes("Streaming"), "profile change applies without reconnect");

  m = await metrics();
  check(m.activeSessions === 1, "gateway reports one active session");
  const sm = m.sessions?.[0] ?? {};
  check(sm.clientRttMs > 0 && sm.clientFps > 0 && sm.inputEvents > 0, "browser stats, pings and input reach the gateway",
    `rtt ${sm.clientRttMs} ms, fps ${sm.clientFps}, input ${sm.inputEvents}`);

  // A replayed token is refused by the gateway.
  const replay = await fetch(`http://127.0.0.1:${SIGNAL}/v1/sessions`, {
    method: "POST", headers: { "Content-Type": "application/json", Origin: ORIGIN },
    body: JSON.stringify({ token: api.lastToken, sdp: "v=0\r\n", hello: { v: ["mea-stream/1"] } }),
  });
  check(replay.status === 401 || replay.status === 403, "replayed token refused", String(replay.status));
  // A token for another desktop is refused.
  const foreign = await fetch(`http://127.0.0.1:${SIGNAL}/v1/sessions`, {
    method: "POST", headers: { "Content-Type": "application/json", Origin: ORIGIN },
    body: JSON.stringify({ token: mint(k.MEA_STREAM_PRIVATE_KEY, "desk-other"), sdp: "v=0\r\n", hello: { v: ["mea-stream/1"] } }),
  });
  check(foreign.status === 404 || foreign.status === 403, "token for an unknown desktop refused", String(foreign.status));

  // Disconnect while holding a key: the key must be released on the desktop.
  await page.locator(".gaming-surface").focus();
  await page.keyboard.down("KeyD");
  await waitFor(() => probe().keys.includes(0x64), 3000, "key d").catch(() => {});
  await page.getByRole("button", { name: "Disconnect" }).click();
  await page.waitForURL("**/dashboard", { timeout: 5000 }).catch(() => {});
  const released = await waitFor(() => !probe().keys.includes(0x64), 5000, "release").catch(() => false);
  check(released, "held key released on disconnect");
  check(api.disconnect >= 1, "MEA disconnect called");
  await waitFor(async () => { m = await metrics(); return m.activeSessions === 0; }, 5000, "slot freed").catch(() => {});
  check(m.activeSessions === 0, "gateway slot freed");

  await browser.close();
  web.close();
}

main()
  .catch((e) => {
    console.log("FAIL", e.message);
    failures++;
  })
  .finally(() => {
    stopping = true;
    if (failures) for (const { name, log } of procs) console.log(`--- ${name} log ---\n${log.join("").slice(-3000)}`);
    for (const { p } of procs) p.kill("SIGTERM");
    console.log(failures ? `${failures} check(s) failed` : "all checks passed");
    process.exit(failures ? 1 : 0);
  });
