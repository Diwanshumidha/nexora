import { useEffect, useState } from "react";
import { Catalog, CodeMode, Connect, Extensions, History, Network, Permissions, type Approval, type ConnectInfo } from "../lib/api";
import { useLive } from "../lib/useLive";
import { Inspector } from "../orbit/Inspector";
import { OrbitMap, type Selection } from "../orbit/OrbitMap";
import { Icon } from "../orbit/icons";

interface Props {
  selected: Selection;
  onSelect: (s: Selection) => void;
  approvals: Approval[];
  refreshApprovals: () => void;
  onOpenRun: (id: string) => void;
  onOpenHistory: () => void;
}

export default function OrbitPage({ selected, onSelect: setSelected, approvals, refreshApprovals, onOpenRun, onOpenHistory }: Props) {
  const [connect, setConnect] = useState<ConnectInfo | null>(null);

  const [extensions, refreshExt] = useLive(async () => (await Extensions.List()) ?? [], 4000, []);
  const [network, refreshNet] = useLive(async () => (await Network.List()) ?? [], 5000, []);
  // A few beyond the orbit's slots: the extension panel's "recent use" and
  // the "all runs" node need to know there are more.
  const [history, refreshHist] = useLive(async () => (await History.Summaries(50)) ?? [], 3000, []);
  const [stats, refreshStats] = useLive(() => CodeMode.QueueStats(), 3000, null);
  const [modes, refreshModes] = useLive(async () => (await Permissions.Modes()) ?? {}, 5000, {});
  // From the saved catalog (no network); the backend re-checks every few hours.
  const [updates, refreshUpdates] = useLive(async () => (await Catalog.Updates()) ?? [], 30000, []);

  useEffect(() => {
    Connect.Info().then(setConnect);
  }, []);

  const refresh = () => {
    refreshExt();
    refreshNet();
    refreshHist();
    refreshStats();
    refreshModes();
    refreshUpdates();
    refreshApprovals();
  };

  const running = extensions.filter((e) => e.status === "running").length;
  const live = network.find((n) => n.active && n.state === "up");

  return (
    <div className="orbit-page">
      <main className="orbit-main">
        <header className="orbit-header">
          <div className="orbit-heading">
            <h1>Your orbit</h1>
            <p>Network providers circle the core, extensions beyond them, and your latest runs on the outer ring. Click anything to inspect it.</p>
          </div>
          <div className="header-stats" aria-live="polite">
            <span className="pill"><span className="live-dot" aria-hidden="true" />{running} extensions live</span>
            <span className="pill">{live ? `reachable via ${live.name}` : "local only"}</span>
          </div>
          <button className="btn primary" onClick={() => setSelected({ kind: "add" })}>
            <Icon name="plus" size={18} /> Add extension
            {updates.length > 0 && <span className="count" title={`${updates.length} update${updates.length > 1 ? "s" : ""} available`}>{updates.length}</span>}
          </button>
        </header>
        <OrbitMap network={network} extensions={extensions} history={history} modes={modes} approvals={approvals} updates={updates.length}
          selected={selected} onSelect={setSelected} onOpenHistory={() => onOpenHistory()} />
      </main>
      <Inspector selected={selected} onSelect={setSelected} connect={connect} stats={stats} approvals={approvals} onOpenRun={onOpenRun}
        network={network} extensions={extensions} history={history} updates={updates} refresh={refresh} />
    </div>
  );
}
