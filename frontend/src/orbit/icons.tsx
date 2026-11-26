// Stroke icons (24×24, currentColor) in the Orrery style.
export const icons = {
  sparkles: "M12 3l1.8 4.7L18.5 9.5l-4.7 1.8L12 16l-1.8-4.7L5.5 9.5l4.7-1.8zM19 15l.8 2.2L22 18l-2.2.8L19 21l-.8-2.2L16 18l2.2-.8z",
  table: "M4 5h16v14H4zM4 10h16M4 15h16M10 5v14",
  document: "M6 3h9l4 4v14H6zM14 3v5h5M9 13h6M9 17h6",
  note: "M5 4h14v12l-4 4H5zM15 20v-4h4M8 9h8M8 13h5",
  pdf: "M6 3h9l4 4v14H6zM14 3v5h5M8.5 17v-4h1.5a1.25 1.25 0 0 1 0 2.5H8.5M13 13v4h1a2 2 0 0 0 0-4zM17.5 13h-1.5v4M16 15h1.25",
  archive: "M4 4h16v4H4zM5 8v12h14V8M10 12h4",
  image: "M4 5h16v14H4zM4 15l4-4 5 5 3-3 4 4M15 9a1 1 0 1 0 0 .01",
  cpu: "M7 7h10v10H7zM10 10h4v4h-4zM9 3v4M15 3v4M9 17v4M15 17v4M3 9h4M3 15h4M17 9h4M17 15h4",
  puzzle: "M10 4a2 2 0 1 1 4 0v2h4v4h-2a2 2 0 1 0 0 4h2v4h-4v-2a2 2 0 1 0-4 0v2H6v-4h2a2 2 0 1 0 0-4H6V6h4z",
  plug: "M9 3v5M15 3v5M6 8h12v3a6 6 0 0 1-12 0zM12 17v4",
  folder: "M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z",
  mail: "M3 6h18v12H3zM3 7l9 6 9-6",
  calendar: "M4 6h16v14H4zM4 10h16M8 3v4M16 3v4",
  globe: "M12 3a9 9 0 1 0 0 18a9 9 0 1 0 0-18M3 12h18M12 3c3 3 3 15 0 18M12 3c-3 3-3 15 0 18",
  code: "M8 8l-4 4 4 4M16 8l4 4-4 4M14 5l-4 14",
  tailscale:
    "M5 5h.01M12 5h.01M19 5h.01M5 12h.01M12 12h.01M19 12h.01M5 19h.01M12 19h.01M19 19h.01",
  cloud: "M7 18h10a4 4 0 0 0 .5-7.97A6 6 0 0 0 6 9.5 4.25 4.25 0 0 0 7 18z",
  shield: "M12 3l8 3v6c0 4.5-3.4 7.8-8 9-4.6-1.2-8-4.5-8-9V6zM9 12l2 2 4-4",
  plus: "M12 5v14M5 12h14",
  orbit: "M12 9a3 3 0 1 0 0 6a3 3 0 1 0 0-6M3 12a9 4 0 1 0 18 0a9 4 0 1 0-18 0",
  terminal: "M4 5h16v14H4zM8 10l3 2-3 2M13 15h4",
  check: "M5 12l5 5 9-10",
  x: "M6 6l12 12M18 6L6 18",
  ai: "M12 3l1.8 4.7L18.5 9.5l-4.7 1.8L12 16l-1.8-4.7L5.5 9.5l4.7-1.8z",
  user: "M12 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8zM4 21a8 8 0 0 1 16 0",
  clock: "M12 3a9 9 0 1 0 0 18a9 9 0 1 0 0-18M12 7v5l3 2",
  copy: "M8 8h12v12H8zM4 16V4h12",
  eye: "M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7S2 12 2 12zM12 9a3 3 0 1 0 0 6a3 3 0 1 0 0-6",
  refresh: "M20 11a8 8 0 1 0-2.3 5.7M20 4v7h-7",
  trash: "M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13",
  hand: "M9 11V5a1.5 1.5 0 0 1 3 0v5M12 10V4a1.5 1.5 0 0 1 3 0v6M15 10V6a1.5 1.5 0 0 1 3 0v8a6 6 0 0 1-6 6h-1a6 6 0 0 1-5-2.7L3.5 13a1.5 1.5 0 0 1 2.5-1.7L9 14",
  ban: "M12 3a9 9 0 1 0 0 18a9 9 0 1 0 0-18M5.6 5.6l12.8 12.8",
  chevronDown: "M6 9l6 6 6-6",
  chevronRight: "M9 6l6 6-6 6",
  search: "M11 4a7 7 0 1 0 0 14a7 7 0 1 0 0-14M20 20l-4-4",
  history: "M3 12a9 9 0 1 0 3-6.7M3 4v4h4M12 8v4l3 2",
} as const;

export type IconName = keyof typeof icons;

export function Icon({ name, size = 20, stroke = 1.7 }: { name: IconName; size?: number; stroke?: number }) {
  // The tailscale dots need a round, thick stroke to render as dots.
  const sw = name === "tailscale" ? 3.2 : stroke;
  return (
    <svg aria-hidden="true" width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor"
      strokeWidth={sw} strokeLinecap="round" strokeLinejoin="round">
      <path d={icons[name]} />
    </svg>
  );
}

export function iconFor(name: string | undefined, fallback: IconName = "puzzle"): IconName {
  return name && name in icons ? (name as IconName) : fallback;
}

/** The Nexora mark: a core with an orbit and a satellite. */
export function Logo({ size = 40 }: { size?: number }) {
  return (
    <svg aria-hidden="true" width={size} height={size} viewBox="0 0 48 48" fill="none">
      <circle cx="24" cy="24" r="21" stroke="var(--accent)" strokeWidth="2.5" />
      <ellipse cx="24" cy="24" rx="21" ry="8" stroke="var(--accent)" strokeWidth="1.6" opacity="0.55"
        transform="rotate(-28 24 24)" />
      <circle cx="24" cy="24" r="6.5" fill="var(--accent)" />
      <circle cx="41" cy="11" r="4" fill="var(--net)" />
    </svg>
  );
}
