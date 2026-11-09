/** Ids of the global panels nanoMuse adds to the harness's `main` seat. */

/** The rooms of the Muse desktop, in the rail's order. */
export const FEED_PANEL = 'nanomuse-feed'
export const IDEAS_PANEL = 'nanomuse-ideas'
export const GOALS_PANEL = 'nanomuse-goals'
export const LIBRARY_PANEL = 'nanomuse-library'

/** The Devices page: this computer on the account's hub and the other devices. */
export const DEVICES_PANEL = 'nanomuse-devices'

/** Settings › Data controls: what the relay keeps of the account's chats, and the only place of the Help-improve switch. */
export const DATA_SECTION = 'nanomuse-data'

export const ROOM_PANELS = [FEED_PANEL, IDEAS_PANEL, GOALS_PANEL, LIBRARY_PANEL, DEVICES_PANEL] as const

/** Where a problem with nanoMuse is reported. */
export const REPO_URL = 'https://github.com/nano-muse/nanoMuse'
export const ISSUES_URL = 'https://github.com/nano-muse/nanoMuse/issues'
export const DOCS_URL = 'https://nanomuse.cn/docs/desktop'
