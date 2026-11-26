// Single import point for the generated Wails bindings, so components don't
// depend on the Go package layout.
export * as Extensions from "../../bindings/nexora/internal/services/extensionservice";
export * as CodeMode from "../../bindings/nexora/internal/services/codemodeservice";
export * as Connect from "../../bindings/nexora/internal/services/connectservice";
export * as Network from "../../bindings/nexora/internal/services/networkservice";
export * as History from "../../bindings/nexora/internal/services/historyservice";
export * as Permissions from "../../bindings/nexora/internal/services/permissionsservice";
export * as Catalog from "../../bindings/nexora/internal/services/catalogservice";
export * as Storage from "../../bindings/nexora/internal/services/storageservice";
export { Choice, Decision, Mode } from "../../bindings/nexora/internal/permissions/models";
export type { Approval } from "../../bindings/nexora/internal/permissions/models";
export type { Info as ExtensionInfo } from "../../bindings/nexora/internal/extension/models";
export type { Job, Stats as QueueStats } from "../../bindings/nexora/internal/jobs/models";
export type { Record as HistoryRecord } from "../../bindings/nexora/internal/history/models";
/** Light view of a run (no script, result, logs or arguments). */
export type { Summary as RunSummary } from "../../bindings/nexora/internal/services/models";
export type { Setting } from "../../bindings/nexora/internal/network/models";
export type {
  CatalogItem,
  CatalogView,
  ConnectInfo,
  StorageEntry,
  StorageView,
  UpdateResult,
  ExtensionPermissions,
  NetworkEndpoint,
  ToolPermission,
} from "../../bindings/nexora/internal/services/models";

/** Shape of Job.result for code-mode jobs (codemode.Result in Go). */
export interface CodeResult {
  value: unknown;
  logs: string[] | null;
  toolCalls: number;
  tools: string[] | null;
}
