import type { IconName } from "../orbit/icons";

// Who started a run: the app ("app"), an AI over MCP ("mcp") or a saved
// script's schedule ("schedule").

export function sourceIcon(source?: string): IconName {
  return source === "mcp" ? "ai" : source === "schedule" ? "clock" : "user";
}

/** Short label for lists: "AI", "Schedule" or "You". */
export function sourceShort(source?: string): string {
  return source === "mcp" ? "AI" : source === "schedule" ? "Schedule" : "You";
}

/** Heading for a run: "AI via MCP", "Scheduled" or "You". */
export function sourceLong(source?: string): string {
  return source === "mcp" ? "AI via MCP" : source === "schedule" ? "Scheduled" : "You";
}

/** Subject of an approval request, e.g. `The scheduled script "Digest"`. */
export function askingWho(source?: string, scriptName?: string): string {
  if (source === "mcp") return scriptName ? `An AI (script "${scriptName}")` : "An AI";
  if (source === "schedule") return `The scheduled script "${scriptName ?? "?"}"`;
  return scriptName ? `Your script "${scriptName}"` : "A script you ran";
}
