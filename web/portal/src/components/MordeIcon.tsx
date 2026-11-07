const paths = {
  grid: 'M4 4h6v6H4zM14 4h6v6h-6zM4 14h6v6H4zM14 14h6v6h-6z',
  monitor: 'M5 5h14a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2ZM8 21h8m-4-3v3',
  spark: 'm12 3 2 6 6 3-6 2-2 7-2-7-6-2 6-3 2-6Zm7 0v3m-1.5-1.5h3',
  folder: 'M3.5 7A2.5 2.5 0 0 1 6 4.5h4l2 2h6A2.5 2.5 0 0 1 20.5 9v8.5a2 2 0 0 1-2 2h-13a2 2 0 0 1-2-2Z',
  user: 'M12 11.5a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7ZM5 20a7 7 0 0 1 14 0',
  target: 'M20.5 12a8.5 8.5 0 1 1-17 0 8.5 8.5 0 0 1 17 0ZM16.5 12a4.5 4.5 0 1 1-9 0 4.5 4.5 0 0 1 9 0ZM12 11v2',
  link: 'm9.5 14.5 5-5M8.5 17.5l-1 1a4 4 0 0 1-5.5-5.8l4-4a4 4 0 0 1 5.7 0m3.8-2.2 1-1a4 4 0 0 1 5.5 5.8l-4 4a4 4 0 0 1-5.7 0',
  key: 'M12 9a4 4 0 1 1-8 0 4 4 0 0 1 8 0Zm-1 3 8 8m-2-4 2-2m-5 1 2-2',
  settings: 'M15 12a3 3 0 1 1-6 0 3 3 0 0 1 6 0ZM12 3v2m0 14v2M3 12h2m14 0h2M5.6 5.6 7 7m10 10 1.4 1.4M18.4 5.6 17 7M7 17l-1.4 1.4M19 12a7 7 0 1 1-14 0 7 7 0 0 1 14 0Z',
  users: 'M12 8a3 3 0 1 1-6 0 3 3 0 0 1 6 0ZM3.5 19a5.5 5.5 0 0 1 11 0M16 5.2a3 3 0 0 1 0 5.6M16 14a4.5 4.5 0 0 1 4.5 4.5',
  chart: 'M4 5v15h16M8 16v-4m4 4V7m4 9v-6m4 6V4',
  shield: 'M12 2.5 19.5 5v6c0 5-3.5 8.2-7.5 10.5C8 19.2 4.5 16 4.5 11V5Zm-3 9 2 2 4-4',
  bell: 'M6 16.5h12l-1.5-2v-4a4.5 4.5 0 0 0-9 0v4ZM10 19a2 2 0 0 0 4 0',
  file: 'M6 3.5h8l4 4v13H6zM14 3.5v4h4M9 12h6m-6 3h6',
} as const;

export type MordeIconName = keyof typeof paths;

export function MordeIcon({ name }: { name: MordeIconName }) {
  return <svg className="morde-icon" viewBox="0 0 24 24" aria-hidden="true" focusable="false"><path d={paths[name]} /></svg>;
}
