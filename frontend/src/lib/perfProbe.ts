// Scroll benchmark for the real desktop webview, run when the app starts
// with NEXORA_PERF=1 (the window opens at #perf). Not used otherwise.
import { Report } from "../../bindings/nexora/internal/services/perfprobe";

const wait = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** Scrolls `el` up and down for `ms`, one step per frame, and measures the
 *  time between frames. */
async function measure(name: string, el: Element | null, ms = 3000) {
  if (!el || el.scrollHeight <= el.clientHeight + 10) return { name, skipped: "not scrollable" };
  const deltas: number[] = [];
  let dir = 1;
  let last = performance.now();
  const end = last + ms;
  await new Promise<void>((done) => {
    const step = (now: number) => {
      deltas.push(now - last);
      last = now;
      const max = el.scrollHeight - el.clientHeight;
      el.scrollTop += 28 * dir;
      if (el.scrollTop >= max || el.scrollTop <= 0) dir = -dir;
      if (now < end) requestAnimationFrame(step);
      else done();
    };
    requestAnimationFrame(step);
  });
  deltas.shift();
  const sorted = [...deltas].sort((a, b) => a - b);
  const avg = deltas.reduce((a, b) => a + b, 0) / deltas.length;
  return {
    name,
    fps: Math.round(1000 / avg),
    avgMs: +avg.toFixed(1),
    p95Ms: +sorted[Math.floor(sorted.length * 0.95)].toFixed(1),
    slowFrames: deltas.filter((d) => d > 25).length,
    frames: deltas.length,
  };
}

/** Runs the benchmark; `goTo` switches the app's page. */
export async function runPerfProbe(goTo: (page: "orbit" | "history") => void) {
  if (location.hash.includes("bare")) {
    // Baseline: a plain list with no app styling, to measure the engine.
    await wait(2000);
    const box = document.createElement("div");
    box.style.cssText = "position:fixed;inset:0;overflow:auto;background:#111;color:#ddd;font:14px sans-serif;z-index:99";
    box.innerHTML = Array.from({ length: 2000 }, (_, i) => `<div style="padding:6px 12px">row ${i}</div>`).join("");
    document.body.appendChild(box);
    await wait(500);
    await Report(JSON.stringify({ variant: location.hash, results: [await measure("bare list", box)] }));
    return;
  }
  if (location.hash.includes("nogrid")) document.body.classList.add("perf-nogrid");
  await wait(2500);
  goTo("history");
  await wait(1500);
  (document.querySelector(".run-item:nth-child(3) button, .run-item") as HTMLElement | null)?.click();
  await wait(1200);
  const results = [
    await measure("history list", document.querySelector(".run-list")),
    await measure("history detail", document.querySelector(".history-detail")),
  ];
  goTo("orbit");
  await wait(1500);
  results.push(await measure("orbit inspector", document.querySelector(".inspector")));
  await Report(JSON.stringify({ variant: location.hash, ua: navigator.userAgent.slice(0, 60), results }));
}
