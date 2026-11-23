import { useEffect, useState } from "react";
import { CodeMode, type CodeResult, type Job, type QueueStats } from "../lib/api";

const sample = `// Scripts run as the body of an async function.
// Only \`tools\` is available: no fs, network or process access.
// Independent calls run in parallel with Promise.all.
const [sum, msg] = await Promise.all([
  tools.hello.add({ a: 2, b: 3 }),
  tools.hello.greet({ name: "Nexora" }),
])
console.log(msg)
return { sum, msg }
`;

export default function PlaygroundPage() {
  const [types, setTypes] = useState("");
  const [code, setCode] = useState(sample);
  const [job, setJob] = useState<Job | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [running, setRunning] = useState(false);
  const [stats, setStats] = useState<QueueStats | null>(null);

  useEffect(() => {
    CodeMode.Types().then(setTypes);
    const refresh = () => CodeMode.QueueStats().then(setStats);
    refresh();
    const timer = setInterval(refresh, 2000);
    return () => clearInterval(timer);
  }, []);

  const run = async () => {
    setRunning(true);
    setError(null);
    try {
      setJob(await CodeMode.Run(code));
    } catch (e) {
      setJob(null);
      setError(String(e));
    } finally {
      setRunning(false);
    }
  };

  const result = job?.result as CodeResult | undefined;
  const logs = result?.logs ?? [];

  return (
    <section className="playground">
      <header className="page-header">
        <h1>Code Mode</h1>
        <div className="header-actions">
          {stats && (
            <span className="muted">
              {stats.running}/{stats.workers} running · {stats.queued} queued
            </span>
          )}
          <button onClick={run} disabled={running}>
            {running ? "Running…" : "Run"}
          </button>
        </div>
      </header>
      <div className="split">
        <div className="pane">
          <h3>Script</h3>
          <textarea spellCheck={false} value={code} onChange={(e) => setCode(e.target.value)} />
        </div>
        <div className="pane">
          <h3>API available to the AI</h3>
          <pre>{types}</pre>
        </div>
      </div>
      <div className="pane">
        <h3>Output</h3>
        {error && <pre className="error">{error}</pre>}
        {job && (
          <pre className={job.state === "succeeded" ? "" : "error"}>
            {logs.join("\n")}
            {logs.length > 0 && "\n\n"}
            {job.state === "succeeded" ? JSON.stringify(result?.value, null, 2) : `${job.state}: ${job.error ?? ""}`}
          </pre>
        )}
      </div>
    </section>
  );
}
