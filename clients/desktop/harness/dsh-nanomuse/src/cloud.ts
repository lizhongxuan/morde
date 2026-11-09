/**
 * nanoMuse Cloud for the harness: `ctx.nanomuseCloud`.
 *
 * One account, every device. A person signs in with a mainland phone number or
 * an e-mail and a six-digit code; the relay answers with this device's key.
 * The key goes to the harness credential store (`NANOMUSE_CLOUD_TOKEN`), and the
 * account's models are written into the profile's `llm-pi-ai` row as the
 * `nanomuse` provider — the same two writes the Models page makes for any
 * OpenAI-compatible gateway — so the harness's own adapter talks to the relay
 * and the models appear in the picker. Nothing of ours sits in the model path.
 *
 * Signed in, this computer is also a device of the account:
 *
 * - the **profile** (`profile.ts`): the agent's name and face come from the
 *   account and follow the phone's avatar studio;
 * - the **hub** (`hub.ts`): one socket to `/v1/hub`, so the phone lists this
 *   computer, this computer's Muse reaches the phone (`reach.ts`), a `notify`
 *   from another device shows here, and — with *remote control* on, as it is
 *   by default — the phone runs things here (`actions.ts`);
 * - the **hands tracker**: every Hands or Reach tool call in flight, so the
 *   browser half can draw the capsule the Android app draws while it works.
 *
 * The browser half (`client/`) drives all of this through a small loopback HTTP
 * API under `/nanomuse/cloud/*` — plain JSON plus one server-sent-events stream
 * (`/events`) carrying the live state — registered when the web server is
 * present; the headless and SDK profiles get the service without routes.
 */
import { mkdir, readFile, rm, writeFile } from 'node:fs/promises'
import type { IncomingMessage, ServerResponse } from 'node:http'
import { isIP } from 'node:net'
import { ApprovalDesk, appOf, type ApprovalScope, isOwnApp, checkForUpdate, compareVersions, DEVICE_GRANT_PREFIX, grantCovers, HoldDesk, modelFor, pickChatModel, pickHandsModel, REMOTE_CONTROL_GRANT_ID, scheduleRunIds, sharedConnectors, standingGrants, takesImages, UPDATE_RETRY_MS, updateCacheFresh, withGrant, type Grant, type Hold, type HoldTool, type PendingApproval, type StandingGrant, type UpdateInfo } from './desk.ts'
import { arch, homedir, hostname, release, type, userInfo } from 'node:os'
import { dirname, isAbsolute, join, resolve } from 'node:path'
import { createHash, createHmac, randomBytes, randomUUID } from 'node:crypto'
import { Service, type Context } from '@deepseek-ai/cordis'
import { credentialRef } from '@deepseek-ai/dsh-credentials'
import type {} from '@deepseek-ai/dsh-host-webserver'
import type {} from '@deepseek-ai/dsh-settings'
import type { ToolDispatchExecution, ToolExecutionResult } from '@deepseek-ai/dsh-tools'
import { createUserMessage, type ContentBlock, type StreamChunk } from '@deepseek-ai/dsh-llm'
import type { Session, SessionEvent, SessionId, AgentCancelCause } from '@deepseek-ai/dsh-session'
import type { SessionCreateRequest } from '@deepseek-ai/dsh-api-session-controller/types'
import type {} from '@deepseek-ai/dsh-api-session-controller'
import type {} from '@deepseek-ai/dsh-user-approval'
import z from '@deepseek-ai/schemastery'
import { mountGuarded } from './admit.ts'
import { brief, REMOTE_ACTIONS, run as runAction, type RemoteAction } from './actions.ts'
import { CODING_ACTIONS, CodingService, codingBrief, GATED_CODING_ACTIONS } from './coding.ts'
import { helloLlm, HubClient, HubError, type CallOptions, type Caller, type HubDevice } from './hub.ts'
import {
  mordeHubCommandAvailable,
  mordeHubCommand,
  mordeHubFileGet,
  mordeHubFiles,
  mordeHubFilesAvailable,
  mordeHubScreenshot,
  mordeHubScreenshotAvailable,
  mordeHubSelectedRoot,
  requireMordeHubTaskGrant,
  type MordeHubFileRoot,
} from './morde-hub-actions.ts'
import { ProfileStore, type Profile } from './profile.ts'
import { cloudOffFailure, relayFailure, transportFailure, type RelayRefusal } from './refusals.ts'
import { Relay, RelayError, type Account, type Estimate, type Invite, type ProfileWrite, type RelayModel, type SignIn } from './relay.ts'
import { TaskRunner, textOf } from './task.ts'
import { Turns, aggregateWork, type WorkItem } from './work.ts'
import { SCREEN_RUN_IDLE_MS, ScreenRuns, Trajectory, type ScreenRun, type StepAction, type TrajectoryView } from './trajectory.ts'
import { describeCall } from './steps.ts'
import { RemoteStore, SyncEngine, SyncRelay, type RemoteLine, type SessionInfo, type SessionLine, type SyncState } from './sync.ts'
import { AvatarMotion, ANIMATED, type MotionMood, type MotionView } from './motion.ts'
import { OwnClip, isOwnClipMood, type OwnClipMood, type OwnClipView } from './own-clip.ts'
import { OwnTurns } from './record.ts'
import { DEFAULT_VIDEO_MODEL, hostOf, KNOWN_DASHSCOPE_MODELS, looksLikeVideoModel, probe as probeVideoModel, speaksDashScope, type VideoEndpoint } from './video.ts'
import { editImage as ownEditImage, generateImage as ownGenerateImage, imageShapeOf, type ImageEndpoint } from './images.ts'
import { checkMove, checkScreenshot, displayInfo, isBlack, runtimeInfo, screenHead, type MoveCheck, type RuntimeInfo, type ScreenshotCheck } from './hands-check.ts'
import { apiOf, baseUrlFor, CAPABILITIES, capabilitiesForAuth, capabilitiesOf, CHATGPT_KEY_REF, CHATGPT_PROVIDER, ChatGptDesk, hasUserinfo, keyRefFor, kindOf, listModelRows, listModels, loadCatalogue, modelsOf, ownProviderRow, parseModelReasoning, regionOf, sighted, typedModelIds, USERINFO_TEXT, type Capability, type ChatGptState, type LoginView, type OwnModel, type OwnProvider, type ProviderEntry, type Region } from './providers.ts'

declare module '@deepseek-ai/cordis' {
  interface Context {
    nanomuseCloud: NanomuseCloud
  }
}

/** The credential reference the `nanomuse` provider row names. */
export const TOKEN_REF = 'NANOMUSE_CLOUD_TOKEN'
/** Morde's installation-bound Client credential, kept apart from the nanoMuse account key. */
export const MORDE_CLIENT_CREDENTIAL = 'MORDE_CLIENT_CREDENTIAL'
/** The provider id inside `llm-pi-ai`; permanent, sessions record it. */
export const PROVIDER_ID = 'nanomuse'
/** The profile row the Models page edits too. */
export const LLM_ROW = 'llm-pi-ai'
/** The `409 chat_on_cloud` body's sentence (the web client shows its own words for the code). */
export const CHAT_ON_CLOUD_MESSAGE = 'Add a provider of your own first; with nanoMuse Cloud off, nothing else could answer.'
/** dsh-base's stock chat default (its `agent-default-model` row): what `adoptOwnDefault` and `adoptDefaultModel` read as "nothing chosen yet". */
export const STOCK_DEFAULT_MODEL = { provider: 'deepseek-official', model: 'deepseek-flash' } as const
/**
 * The image budget of one request through the relay (`dsh-llm-pi-ai` profile keys): the base64
 * images of a request are held to 2 MiB, the oldest offloaded (dsh puts a line in their place),
 * and each image is scaled to 2 Mpx before it is encoded. `requestImageMaxBytes` keeps dsh's
 * default (1 MiB a picture). The byte bound is the only count dsh's adapter takes, and it is
 * set for the hands: a run on a screen adds a picture per step, a 1600-wide screenshot is
 * 200 to 500 KB of base64, and the relay's calls showed desktop hands turns carrying up to 23
 * of them (13 at the 95th percentile), which the model has to read again at every step; the
 * runtime's own loop keeps the newest four (`agent.max_context_images`). 2 MiB keeps about
 * the newest four to eight, and stays far under the relay's body cap (`MAX_REQUEST_BYTES`,
 * 16 MB by default; over it, 413 `too_large`).
 */
export const IMAGE_BUDGET = { maxRequestImageBytes: 2 * 1024 * 1024, requestImagePixelBudget: 2 * 1024 * 1024 } as const
/** Where the browser half talks to us. */
export const API_PREFIX = '/nanomuse/cloud'
/** The public relay no longer offers account-funded image or video generation. */
export const OFFICIAL_CLOUD_ORIGIN = 'https://cloud.nanomuse.cn'

/** Self-hosted relays may still advertise media routes; the public service never does. */
export function relayMediaEnabled(baseURL: string): boolean {
  try {
    return new URL(baseURL).origin !== OFFICIAL_CLOUD_ORIGIN
  } catch {
    return false
  }
}

/** The bundle's version alone, `0.1.41`: what the update check compares with the latest release. */
export const BUNDLE_VERSION = process.env.NANOMUSE_VERSION ?? '0.0.0'
/** What this device reports as its software. */
export const VERSION = `dsh-nanomuse ${BUNDLE_VERSION}`
/** The hub actions this computer answers whatever the remote-control switch says (`docs/hub.md`). */
export const ACTIONS = ['info', 'notify']
/** Incoming actions worth a toast — the ones that act or look, not a folder listing. */
const TOAST_ACTIONS = new Set(['shell', 'file.get', 'file.put', 'open', 'screen'])

export interface Config {
  /** The relay origin. */
  baseURL: string
  /** How this device introduces itself to the account's device list. */
  deviceName: string
  /** Where the account snapshot lives (`$DSH_HOME/nanomuse/cloud.json` by default). */
  statePath: string
}

export const Config: z<Config> = z.object({
  baseURL: z.string().default('https://cloud.nanomuse.cn').description('The nanoMuse Cloud relay.'),
  deviceName: z.string().default('').description('This device in the account\'s device list; empty means the host name.'),
  statePath: z.string().default('').description('Account snapshot file; empty means $DSH_HOME/nanomuse/cloud.json.'),
})

/** What the UI shows; never the key. */
export interface CloudStatus {
  signedIn: boolean
  /** A model can answer: the account's, or a key the person entered in Models. */
  ready: boolean
  baseURL: string
  account?: Account
  models: RelayModel[]
  /** The account chat model new chats use; empty when another provider is the default. */
  chatModel: string
  /** The model the hands see the screen with; empty when the account has none. */
  handsModel: string
  profile: Profile
  hub: HubState
  /** The last relay failure, for the card to show. */
  error?: { code: string; message: string }
}

/** The separate Morde Client Installation login, never the nanoMuse Cloud account. */
export interface MordeHubStatus {
  signedIn: boolean
  connected: boolean
  baseURL: string
  deviceID?: string
  devices: MordeHubDevice[]
  targetGrantVersion: number
  grants: { screenshot: boolean; files: boolean; command: boolean; fileRoot: string | null }
  capabilities: { screenshot: boolean; files: boolean; command: boolean }
  accountName?: string
  installationID?: string
  expiresAt?: string
  lastError?: string
}

export interface MordeHubDevice {
  id: string
  name: string
  platform: 'desktop' | 'android' | 'ios'
  online: boolean
  revoked: boolean
  expiresAt: string
}

export interface MordePersonalObject {
  object_type: string
  object_id: string
  revision: number
  body: Record<string, unknown>
}

export interface MordePersonalMutationReceipt {
  object_type: string
  object_id: string
  result_revision: number
  account_change_sequence: number
  replay: boolean
}

export interface MordeAvailableModel {
  id: string
  name: string
  model_name: string
  capabilities: { text: boolean; tools: boolean; vision: boolean }
}

export type MordeScheduleTiming =
  | { kind: 'once'; run_at: string; time_zone: string }
  | { kind: 'daily'; starts_on: string; local_time: string; time_zone: string }
  | { kind: 'weekly'; starts_on: string; local_time: string; time_zone: string; weekdays: number[] }
  | { kind: 'every_n_hours'; interval_hours: number; time_zone: string; anchor_at?: string }

export interface MordeScheduleCreateInput {
  title?: string
  model_id: string
  objective: string
  host_id: null
  timing: MordeScheduleTiming
  budget: { active_seconds: number; tool_calls: number; model_tokens: number }
  misfire_policy: 'skip'
  notification_events: ['completed', 'failed', 'waiting_user']
  observation: null
}

export interface MordeScheduleReceipt {
  id: string
  version: number
  status: string
  title: string
  timing: MordeScheduleTiming
  next_run_at: string | null
}

export interface MordeGoalPage {
  items: MordePersonalObject[]
  next_cursor: string | null
}

export interface MordePersonalSnapshotPage {
  watermark: number
  items: MordePersonalObject[]
  next_cursor: string | null
}

interface MordeHubState {
  baseURL: string
  targetGrantVersion: number
  grants: { screenshot: boolean; files: boolean; command: boolean }
  fileRoot?: MordeHubFileRoot
  clientInstanceID?: string
  attemptGeneration: number
  accountID?: string
  accountName?: string
  installationID?: string
  expiresAt?: string
}

export interface HubState {
  connected: boolean
  deviceId: string
  deviceName: string
  /**
   * On: every device of the account may run things here (`shell`, `files`, `open`, `screen`, a task) without
   * asking. Off (the default): a device asks the person at this computer first — a card on this screen,
   * allowed once or always for that device. `info` and `notify` always work.
   */
  remoteControl: boolean
  /** Devices the person allowed without asking, while remote control is off. */
  trusted: TrustedDevice[]
  /** Questions from other devices waiting for an answer on this screen. */
  asks: Ask[]
  lastError?: string
  devices: HubDevice[]
}

export interface TrustedDevice {
  id: string
  name: string
  at: number
}

/** Another device wants to run something here; the person answers on this screen. */
export interface Ask {
  id: string
  from: string
  fromId: string
  action: string
  text: string
  at: number
  onceOnly?: boolean
}

export type AskAnswer = 'once' | 'always' | 'deny'
type AskResult = AskAnswer | 'timeout' | 'cancelled'

/** How long a question from another device waits on this screen (the hub call itself waits about as long). */
export const ASK_TIMEOUT_MS = 110_000

/**
 * The questions other devices are waiting on, and the person's answers. `ask` settles with the answer, or
 * `'timeout'` when nobody answered in time; `onChange` fires whenever the list changes (for the live state).
 */
export class AskDesk {
  private readonly pending = new Map<string, { ask: Ask; finish(answer: AskResult): void }>()

  constructor(private readonly onChange: () => void = () => undefined) {}

  get list(): Ask[] {
    return [...this.pending.values()].map((p) => p.ask)
  }

  ask(from: Caller, action: string, text: string, timeoutMs = ASK_TIMEOUT_MS, onceOnly = false, signal?: AbortSignal): Promise<AskResult> {
    const ask: Ask = { id: `ask_${randomUUID().slice(0, 8)}`, from: from.name || 'a device', fromId: from.id, action, text, at: Date.now(), ...(onceOnly ? { onceOnly: true } : {}) }
    return new Promise((resolve) => {
      const finish = (answer: AskResult) => {
        clearTimeout(timer)
        signal?.removeEventListener('abort', onAbort)
        this.pending.delete(ask.id)
        this.onChange()
        resolve(answer)
      }
      const onAbort = () => finish('cancelled')
      if (signal?.aborted) return resolve('cancelled')
      const timer = setTimeout(() => finish('timeout'), timeoutMs)
      this.pending.set(ask.id, { ask, finish })
      signal?.addEventListener('abort', onAbort, { once: true })
      this.onChange()
    })
  }

  /** The person's answer; false when the question is no longer waiting. */
  answer(id: string, answer: AskAnswer): boolean {
    const p = this.pending.get(id)
    if (!p) return false
    p.finish(answer)
    return true
  }
}

/** One Hands or Reach tool call in flight. */
export interface HandsCall {
  id: string
  name: string
  /** The few arguments the capsule reads: action, label, text, device, command, task. */
  args: Record<string, string>
  sessionId: string
  since: number
}

/** A `notify` from another device, or something another device did here — shown as a toast. */
export interface Notice {
  id: number
  /** `notify`: words for the person; `call`: `action` ran here on `from`'s behalf. */
  kind: 'notify' | 'call'
  from: string
  title: string
  text: string
  action?: string
  at: number
}

/** The last thing the hands did, for the stage's caption and marker; since 0.1.40 also a drag's far end (`x2`, `y2`) and a scroll's `dy`. */
export type StageAction = StepAction

/**
 * The Live stage: the latest screenshot the agent took while using a screen —
 * this computer's through the hands, or another device's through Reach — so
 * the person can watch it work. The bytes are served separately (`/stage/frame`).
 */
export interface StageState {
  /** 0 before any frame; grows with each new one (the frame URL's cache key). */
  seq: number
  at: number
  source: 'computer' | 'device'
  /** The other device's name; empty for this computer. */
  device: string
  width: number
  height: number
  /** What is in front on that screen, as the hands reported it. */
  title: string
  /** `window`: the hands drive one app's window and the pointer stays with the person (runtime 0.1.34); `screen` otherwise. */
  mode: 'screen' | 'window'
  action: StageAction | null
  sessionId: string
}

/** The live state the browser half mirrors over `/events`. */
export interface LiveState {
  cloud: { signedIn: boolean; hint: string }
  profile: Profile
  hub: HubState
  mordeHub?: MordeHubStatus
  /**
   * The calls in flight, the step count, and `run`: the session that has this computer's
   * screen, from its first `computer_*` call to its turn's end (null between runs). The
   * shell's glow and capsule follow `run`, so they stay up while the model thinks.
   */
  hands: { calls: HandsCall[]; steps: number; run: ScreenRun | null }
  stage: StageState
  notices: Notice[]
  /** Questions the agent asked before a step, open right now (C2); the stage answers them too. */
  approvals: PendingApproval[]
  /** Pauses of the hands that are on (C1). */
  holds: Hold[]
  /** Standing "always allow" grants for the hands, per app. */
  grants: Grant[]
  /** The tasks some of those grants are limited to that the schedule service no longer has. */
  goneSchedules: string[]
  /** The titles of the scheduled tasks in each chat a grant is limited to, by session id. */
  chatTasks: Record<string, string[]>
  /** The model the hands see the screen with (`[gui]` of the bundled runtime); empty when the account has none. */
  handsModel: string
  /** The last update check, when one ran. */
  update: UpdateInfo | null
  /** The face's clips: which exist (with a cache key) and how the drawing goes (desk-b). */
  motion: MotionView
  /** The person's own clip as the face (#324): its size, cache key and whether a first frame was kept; null for none. Absent on an older host. */
  ownClip?: OwnClipView | null
  /** Local video overrides for individual moods. Older clients ignore this field. */
  ownClips?: Partial<Record<OwnClipMood, OwnClipView>>
  /** When the hands last saw an all-black screen (macOS: Screen Recording missing, or granted after the app started); 0 when they have not. */
  blackScreenAt: number
  /** Conversation sync (C8): `rev` moves with every change pulled or pushed; the session that is the account's main conversation. */
  sync: { rev: number; mainSession: string }
  /** Own keys and the ChatGPT sign-in (C11): how many rows, what the sign-in is doing, whether its proxy is up. */
  ownKeys: { count: number; capabilities: Capability[]; chatgpt: { signedIn: boolean; label: string; proxy: boolean; login: LoginView } }
  /** The hands' trajectory (0.1.40): `rev` moves with every step; the sessions that have a run to look back at. The runs themselves are `GET /trajectory?session=`. */
  trajectory: { rev: number; sessions: string[] }
  /** The 80 % heads-up (C12), while it is due: what is left of the pool and what an invitation adds, in yuan; null otherwise. */
  headsUp: { left: number; grant: number; bonus: number } | null
  /** The account's model calls still without a first token (0.31): when each began and its session, for the "waited N s" line above that chat's composer. */
  waiting: Array<{ since: number; session: string }>
}

/** What Settings → Account's "ways on", the own-key step and the pickers read (`GET /providers`). */
export interface ProvidersView {
  region: Region
  catalogue: ProviderEntry[]
  /** The rows the person has (never a key). */
  configured: OwnProvider[]
  /** What everything configured can do between them: the account while signed in, the rows, the ChatGPT sign-in. */
  capabilities: Capability[]
  cloud: { signedIn: boolean; capabilities: Capability[] }
  chatgpt: { signedIn: boolean; label: string; proxy: boolean; login: LoginView; runtime: boolean }
  /** The hands model and where it comes from: `nanomuse` for the account, else an own row's id. */
  hands: { provider: string; model: string }
  chat: { provider: string; model: string }
}

/** One choice in a model picker: a model of the account (`provider` = `nanomuse`) or of an own row. */
export interface ModelOption {
  provider: string
  providerLabel: string
  id: string
  name: string
  /** The relay's recommended one for that lane (marked in the picker). */
  recommended?: boolean
}

/** How the last mount of the hands' MCP client went (`hands-tools.ts`); `at` 0 before the first. */
export interface HandsMount {
  at: number
  ok: boolean
  /** Empty when the mount went well; else the error, in the host's words. */
  reason: string
}

/** The four slots of Settings → Models (0.1.41). */
export type Slot = 'chat' | 'hands' | 'image' | 'video'
export const SLOTS: readonly Slot[] = ['chat', 'hands', 'image', 'video']

/** One row of Settings → Models: what is in use, where it lives, and what the picker lists. */
export interface SlotView {
  /** `nanomuse` for the account, an own row's id, something else of dsh's, or '' for nothing. */
  provider: string
  /** The provider's label as the row shows it (`nanoMuse Cloud`, the row's label); '' for nothing. */
  providerLabel: string
  model: string
  options: ModelOption[]
  /** The person chose this (an explicit choice); false when it is the resolution order's pick. */
  chosen: boolean
  /** The clips are switched off (the video slot only). */
  off?: boolean
  /** What *Automatic* resolves to right now (the three dependent slots): the order without the stored choice; `model` '' when nothing can. */
  auto?: { provider: string; providerLabel: string; model: string }
}

/** `GET /models`: the page in one read. */
export interface ModelsView {
  signedIn: boolean
  /** Settings › Models › *Use nanoMuse Cloud models*: signed in and not switched off. */
  cloudModels: boolean
  slots: Record<Slot, SlotView>
  /** Own rows with sighted models the hands cannot use: the runtime speaks OpenAI's shape only (their labels). */
  handsExcluded: string[]
}

/** The "Use it for" card after a key is saved: the slots the row could take and the model each would get. */
export type SlotOffer = Partial<Record<Slot, string>>

/** The session-local switch behind *Use nanoMuse Cloud this time*: the selection to put back when the turn ends. */
interface CloudOnce {
  provider: string
  model: string
  reasoningEffort?: string
  at: number
}

/** How long after the last hands call the stage keeps its frame. */
const STAGE_REST_MS = 10 * 60_000
/** Four candidates and four poses (the idle still is the candidate itself): what a new face costs. */
const STUDIO_PICTURES = 8

const NO_STAGE: StageState = { seq: 0, at: 0, source: 'computer', device: '', width: 0, height: 0, title: '', mode: 'screen', action: null, sessionId: '' }

interface State {
  account?: Account
  models?: RelayModel[]
  deviceId?: string
  deviceName?: string
  /** `true` lets every device of the account run things here without asking; absent or `false` means ask. */
  remoteControl?: boolean
  /** Devices allowed without asking (device id → name and when), while remote control is off. */
  trusted?: Record<string, { name: string; at: number }>
  /** The session each other device's conversation lives in (`<device id>\n<conversation>` → session id). */
  taskSessions?: Record<string, string>
  /** Conversations synced between the account's devices (C7, C8): the cursor, the ids, what the relay knows. */
  sync?: SyncState
  /** The hands model the person chose; absent means the account's default. */
  handsModel?: string
  /** Where the chosen hands model lives (C11): an own row's id; absent means the account. */
  handsProvider?: string
  /** "Always allow" given on the stage, per app. */
  grants?: Grant[]
  /** The last update check and when it ran, so the badge survives a restart and the check runs once a day. */
  update?: UpdateInfo
  /** Settings → Media (desk-b): the video model and whether a new face is animated. */
  media?: MediaState
  /** Own-key providers written by the plugin (C11), by provider row id; the keys are in the credential store. */
  providers?: Record<string, OwnProvider>
  /** One-time removal of the desktop's old own-provider routes and their exact credential refs. */
  ownProviderKeyCleanup?: { version: 1; status: 'pending' | 'complete'; targets?: unknown }
  /** The ChatGPT sign-in (C11): a subscription signed in through the runtime's `nanomuse chatgpt` flow. */
  chatgpt?: ChatGptState
  /** The pool size (yuan) the 80 % heads-up was shown for; it comes back only once the pool has grown. */
  warnedGrant?: number
  /**
   * Settings › Models › *Use nanoMuse Cloud models* switched off: the account's models leave
   * every slot's order and list, no side call goes through the relay, and a chat that still
   * sits on a Cloud model is refused with a card. The sign-in stays (sync, the hub, the
   * account page). Only *Use nanoMuse Cloud this time* spends the allowance while this is set.
   */
  cloudModelsOff?: boolean
  /** Server and ordered-login metadata only; the Client credential lives in Credential Store. */
  mordeHub?: MordeHubState
}

interface OwnProviderKeyCleanupTarget {
  id: string
  provider: string
  keyRef: string
}

function mordeBaseURL(value: string): string {
  let url: URL
  try {
    url = new URL(value.trim())
  } catch {
    throw new RelayError(400, 'invalid_base_url', 'Enter a valid Morde server address')
  }
  const host = url.hostname.toLowerCase().replace(/^\[|\]$/g, '')
  const loopback = host === 'localhost' || host === '::1' || (isIP(host) === 4 && host.startsWith('127.'))
  if ((url.protocol !== 'https:' && !(url.protocol === 'http:' && loopback)) || url.username || url.password || url.search || url.hash || value.length > 2048) {
    throw new RelayError(400, 'invalid_base_url', 'Use HTTPS, or HTTP on this computer only')
  }
  return `${url.origin}${url.pathname.replace(/\/+$/, '')}`
}

function mordeAPIURL(baseURL: string, route: string): string {
  const base = new URL(baseURL)
  return new URL(`${base.pathname.replace(/\/+$/, '')}/api/${route}`, base.origin).href
}

function mordeHubURL(baseURL: string): string {
  const url = new URL(mordeAPIURL(baseURL, 'hub'))
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
  return url.href
}

function mordeHubDevicesOf(value: unknown): MordeHubDevice[] {
  const items = value && typeof value === 'object' && !Array.isArray(value) ? (value as { items?: unknown }).items : undefined
  if (!Array.isArray(items) || items.length > 256) throw new RelayError(502, 'bad_response', 'Morde server sent an invalid device list')
  return items.map((item) => {
    const device = item && typeof item === 'object' && !Array.isArray(item) ? item as Record<string, unknown> : {}
    if (typeof device.id !== 'string' || !/^i[a-z0-9]{7}$/.test(device.id) ||
      typeof device.display_name !== 'string' || !device.display_name.trim() || Buffer.byteLength(device.display_name) > 100 ||
      !['desktop', 'android', 'ios'].includes(String(device.platform)) || typeof device.online !== 'boolean' ||
      typeof device.revoked !== 'boolean' || typeof device.expires_at !== 'string' || !Number.isFinite(Date.parse(device.expires_at))) {
      throw new RelayError(502, 'bad_response', 'Morde server sent an invalid device list')
    }
    return {
      id: device.id, name: device.display_name, platform: device.platform as MordeHubDevice['platform'],
      online: device.online, revoked: device.revoked, expiresAt: new Date(device.expires_at).toISOString(),
    }
  })
}

function mordeHubStateOf(value: unknown): MordeHubState | undefined {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return undefined
  const raw = value as Record<string, unknown>
  if (typeof raw.baseURL !== 'string') return undefined
  let baseURL: string
  try {
    baseURL = mordeBaseURL(raw.baseURL)
  } catch {
    return undefined
  }
  const generation = Number(raw.attemptGeneration)
  const grants = raw.grants && typeof raw.grants === 'object' && !Array.isArray(raw.grants)
    ? raw.grants as Record<string, unknown>
    : {}
  const savedRoot = raw.fileRoot && typeof raw.fileRoot === 'object' && !Array.isArray(raw.fileRoot)
    ? raw.fileRoot as Record<string, unknown>
    : {}
  const fileRoot = typeof savedRoot.directory === 'string' && savedRoot.directory.length <= 4096 &&
      isAbsolute(savedRoot.directory) && resolve(savedRoot.directory) === savedRoot.directory &&
      typeof savedRoot.identity === 'string' && /^[A-Za-z0-9_-]{16,256}$/.test(savedRoot.identity)
    ? { directory: savedRoot.directory, identity: savedRoot.identity }
    : undefined
  const requestedVersion = Number(raw.targetGrantVersion)
  return {
    baseURL,
    targetGrantVersion: Number.isSafeInteger(requestedVersion) && requestedVersion > 0 ? requestedVersion : 1,
    grants: {
      screenshot: grants.screenshot === true,
      files: grants.files === true && Boolean(fileRoot),
      command: grants.command === true && Boolean(fileRoot),
    },
    ...(fileRoot ? { fileRoot } : {}),
    ...(typeof raw.clientInstanceID === 'string' && /^[0-9a-f-]{36}$/i.test(raw.clientInstanceID) ? { clientInstanceID: raw.clientInstanceID } : {}),
    attemptGeneration: Number.isSafeInteger(generation) && generation >= 0 ? generation : 0,
    ...(typeof raw.accountID === 'string' ? { accountID: raw.accountID.slice(0, 128) } : {}),
    ...(typeof raw.accountName === 'string' ? { accountName: raw.accountName.slice(0, 100) } : {}),
    ...(typeof raw.installationID === 'string' ? { installationID: raw.installationID.slice(0, 128) } : {}),
    ...(typeof raw.expiresAt === 'string' && Number.isFinite(Date.parse(raw.expiresAt)) ? { expiresAt: new Date(raw.expiresAt).toISOString() } : {}),
  }
}

async function mordeRequest<T>(baseURL: string, route: string, init: RequestInit = {}, maxResponseBytes = 64 * 1024): Promise<T> {
  const response = await fetch(mordeAPIURL(baseURL, route), {
    ...init,
    redirect: 'error',
    signal: AbortSignal.timeout(30_000),
  })
  if (response.status === 204) return undefined as T
  const text = await response.text()
  if (Buffer.byteLength(text) > maxResponseBytes) throw new RelayError(502, 'bad_response', 'Morde server response is too large')
  let body: unknown
  try {
    body = text ? JSON.parse(text) : undefined
  } catch {
    body = undefined
  }
  if (!response.ok) {
    const error = body && typeof body === 'object' ? (body as { error?: { code?: unknown; message?: unknown } }).error : undefined
    throw new RelayError(
      response.status,
      typeof error?.code === 'string' ? error.code.slice(0, 64) : `http_${response.status}`,
      typeof error?.message === 'string' ? error.message.slice(0, 500) : `${response.status} ${response.statusText}`.trim(),
    )
  }
  if (!body || typeof body !== 'object' || Array.isArray(body)) throw new RelayError(502, 'bad_response', 'Morde server sent an invalid response')
  return body as T
}

function mordeObjectSegment(value: string): string {
  if (typeof value !== 'string' || !/^[A-Za-z0-9._-]{1,128}$/.test(value) || value === '.' || value === '..') {
    throw new RelayError(400, 'invalid_personal_object', 'The Morde personal object is invalid')
  }
  return encodeURIComponent(value)
}

function validateMordeMutation(baseRevision: number, mutationID: string): void {
  if (!Number.isSafeInteger(baseRevision) || baseRevision < 0 || typeof mutationID !== 'string' ||
    !/^[A-Za-z0-9._~-]{8,128}$/.test(mutationID)) {
    throw new RelayError(400, 'invalid_personal_mutation', 'The Morde personal mutation is invalid')
  }
}

function mordePersonalObjectOf(value: unknown): MordePersonalObject {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new RelayError(502, 'bad_response', 'Morde server sent an invalid personal object')
  const raw = value as Record<string, unknown>
  if (typeof raw.object_type !== 'string' || typeof raw.object_id !== 'string' ||
    !Number.isSafeInteger(raw.revision) || Number(raw.revision) < 1 ||
    !raw.body || typeof raw.body !== 'object' || Array.isArray(raw.body)) {
    throw new RelayError(502, 'bad_response', 'Morde server sent an invalid personal object')
  }
  return { object_type: raw.object_type, object_id: raw.object_id, revision: Number(raw.revision), body: raw.body as Record<string, unknown> }
}

function mordeAvailableModelsOf(value: unknown): MordeAvailableModel[] {
  if (!value || typeof value !== 'object' || Array.isArray(value) || !Array.isArray((value as Record<string, unknown>).items)) {
    throw new RelayError(502, 'bad_response', 'Morde server sent an invalid model list')
  }
  return ((value as { items: unknown[] }).items).flatMap((item) => {
    if (!item || typeof item !== 'object' || Array.isArray(item)) return []
    const raw = item as Record<string, unknown>
    const capabilities = raw.capabilities && typeof raw.capabilities === 'object' && !Array.isArray(raw.capabilities)
      ? raw.capabilities as Record<string, unknown>
      : undefined
    if (typeof raw.id !== 'string' || !raw.id || typeof raw.name !== 'string' || typeof raw.model_name !== 'string' ||
      !capabilities || typeof capabilities.text !== 'boolean' || typeof capabilities.tools !== 'boolean' || typeof capabilities.vision !== 'boolean') {
      throw new RelayError(502, 'bad_response', 'Morde server sent an invalid model list')
    }
    return [{ id: raw.id, name: raw.name, model_name: raw.model_name, capabilities: { text: capabilities.text, tools: capabilities.tools, vision: capabilities.vision } }]
  })
}

function mordeScheduleReceiptOf(value: unknown): MordeScheduleReceipt {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new RelayError(502, 'bad_response', 'Morde server sent an invalid schedule')
  const raw = value as Record<string, unknown>
  const timing = raw.timing && typeof raw.timing === 'object' && !Array.isArray(raw.timing) ? raw.timing as MordeScheduleTiming : null
  const nextRunAt = raw.next_run_at
  if (typeof raw.id !== 'string' || !raw.id || !Number.isSafeInteger(raw.version) || Number(raw.version) < 1 ||
    raw.status !== 'enabled' || typeof raw.title !== 'string' || !raw.title.trim() || Buffer.byteLength(raw.title, 'utf8') > 32768 ||
    !timing || typeof timing.kind !== 'string' || !Object.hasOwn(raw, 'next_run_at') ||
    (nextRunAt !== null && (typeof nextRunAt !== 'string' || !Number.isFinite(Date.parse(nextRunAt))))) {
    throw new RelayError(502, 'bad_response', 'Morde server sent an invalid or disabled schedule response')
  }
  return { id: raw.id, version: Number(raw.version), status: raw.status, title: raw.title, timing, next_run_at: nextRunAt as string | null }
}

function mordeMutationReceiptOf(value: unknown): MordePersonalMutationReceipt {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new RelayError(502, 'bad_response', 'Morde server sent an invalid mutation receipt')
  const raw = value as Record<string, unknown>
  if (typeof raw.object_type !== 'string' || typeof raw.object_id !== 'string' ||
    !Number.isSafeInteger(raw.result_revision) || Number(raw.result_revision) < 1 ||
    !Number.isSafeInteger(raw.account_change_sequence) || Number(raw.account_change_sequence) < 0 ||
    typeof raw.replay !== 'boolean') {
    throw new RelayError(502, 'bad_response', 'Morde server sent an invalid mutation receipt')
  }
  return {
    object_type: raw.object_type, object_id: raw.object_id, result_revision: Number(raw.result_revision),
    account_change_sequence: Number(raw.account_change_sequence), replay: raw.replay,
  }
}

function mordeGoalPageOf(value: unknown): MordeGoalPage {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new RelayError(502, 'bad_response', 'Morde server sent an invalid Goal page')
  const raw = value as Record<string, unknown>
  if (!Array.isArray(raw.items) || raw.items.length > 100 ||
    (raw.next_cursor !== null && typeof raw.next_cursor !== 'string')) {
    throw new RelayError(502, 'bad_response', 'Morde server sent an invalid Goal page')
  }
  const items = raw.items.map(mordePersonalObjectOf)
  if (items.some((item) => item.object_type !== 'goal')) throw new RelayError(502, 'bad_response', 'Morde server sent an invalid Goal page')
  return { items, next_cursor: raw.next_cursor as string | null }
}

function mordePersonalSnapshotOf(value: unknown): MordePersonalSnapshotPage {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new RelayError(502, 'bad_response', 'Morde server sent an invalid Personal snapshot')
  const raw = value as Record<string, unknown>
  if (!Number.isSafeInteger(raw.watermark) || Number(raw.watermark) < 0 || !Array.isArray(raw.items) || raw.items.length > 100 ||
    (raw.next_cursor !== null && typeof raw.next_cursor !== 'string')) {
    throw new RelayError(502, 'bad_response', 'Morde server sent an invalid Personal snapshot')
  }
  return { watermark: Number(raw.watermark), items: raw.items.map(mordePersonalObjectOf), next_cursor: raw.next_cursor as string | null }
}

/** How often, at most, the account is re-read after a turn for the heads-up. */
const ALLOWANCE_CHECK_EVERY_MS = 60_000

/** The Media settings as stored. `videoModel` empty means the endpoint's default; `off` means no clips. */
export interface MediaState {
  videoModel?: string
  /** Where the chosen video model lives (0.1.41): `nanomuse` or an own row's id; absent with a `videoModel` means whichever source lists it (0.1.40 and before). */
  videoProvider?: string
  /** The image model the person chose (0.1.41) and where it lives; absent means the resolution order's pick. */
  imageModel?: string
  imageProvider?: string
  /** Absent means on. */
  animate?: boolean
  /** The last check of which video models the own key reaches: by host, with the models and when. */
  checked?: Record<string, { models: string[]; at: number }>
}

/** The `videoModel` value that means "no clips" (the phone's `VIDEO_OFF`). */
export const VIDEO_OFF = 'off'
/** How long a video-model check against a provider is trusted. */
const VIDEO_CHECK_TTL_MS = 24 * 60 * 60_000

/** What Settings → Media shows. */
export interface MediaView {
  /** The image model the account would draw with (Cloud), empty when signed out or none. */
  imageModel: string
  /** Pictures (C11): where they would be drawn — the account, an own row with image models, or nowhere (`reason` `no_image`). */
  image: { source: 'cloud' | 'provider' | 'none'; label: string; model: string; reason: string }
  /** The video source: `cloud`, an own-key provider, or none (`reason`: `no_video` when nothing configured has video models). */
  video: { source: 'cloud' | 'provider' | 'none'; label: string; model: string; models: { id: string; name: string }[]; off: boolean; reason: string }
  animate: boolean
  motion: MotionView
}

/**
 * A cached check, read against the bundle that runs now. The state file outlives an
 * update: right after 1.0.2 was installed over 1.0.1, the check 1.0.1 made within the day
 * still said `{ current: '1.0.1', latest: '1.0.2', newer: true }`, and the sidebar offered
 * "Update to 1.0.2" to the 1.0.2 that was running until the next daily check (#341). So
 * `current` becomes this bundle's version and `newer` is compared again against the cached
 * `latest`; the time of the check is kept, so no extra request is made. A cache made by
 * this very version is returned as it is.
 */
export function updateForThisVersion(cached: UpdateInfo, current: string = BUNDLE_VERSION): UpdateInfo {
  if (cached.current === current) return cached
  const latest = typeof cached.latest === 'string' ? cached.latest : ''
  return { ...cached, current, latest, newer: Boolean(latest) && compareVersions(latest, current) > 0 }
}

/** Tool names whose calls the capsule follows. */
export function isHandsTool(name: string): boolean {
  return name.startsWith('mcp__nanomuse__') || name === 'devices' || name.startsWith('device_') || name === 'delegate'
}

/** The trusted-device map as stored, dropping anything malformed. */
function trustedOf(raw: Record<string, unknown>): Record<string, { name: string; at: number }> {
  const out: Record<string, { name: string; at: number }> = {}
  for (const [id, v] of Object.entries(raw)) {
    if (!id || !v || typeof v !== 'object') continue
    const { name, at } = v as { name?: unknown; at?: unknown }
    out[id] = { name: typeof name === 'string' && name ? name : id, at: typeof at === 'number' ? at : 0 }
  }
  return out
}

/** What a hand-made endpoint is called when the person gives it no name: its host, with the port when it has one. */
function hostNameOf(url: string): string {
  try {
    return new URL(url).host
  } catch {
    return url
  }
}

/** The own-key rows as `cloud.json` has them: malformed ones dropped, never a key (the credential store has those). */
function ownProvidersOf(raw: Record<string, unknown>): Record<string, OwnProvider> {
  const out: Record<string, OwnProvider> = {}
  for (const [id, value] of Object.entries(raw)) {
    if (!value || typeof value !== 'object') continue
    const p = value as Partial<OwnProvider>
    if (typeof p.baseURL !== 'string' || !Array.isArray(p.capabilities)) continue
    out[id] = {
      // the key is the id; a row written before the endpoint list (#294) carries none of its own
      id,
      provider: typeof p.provider === 'string' && p.provider ? p.provider : id,
      label: typeof p.label === 'string' && p.label ? p.label : id,
      protocol: p.protocol === 'anthropic' || p.protocol === 'gemini' || p.protocol === 'openai-responses' ? p.protocol : 'openai',
      baseURL: p.baseURL,
      keyRef: typeof p.keyRef === 'string' ? p.keyRef : '',
      capabilities: p.capabilities.filter((c): c is Capability => (CAPABILITIES as readonly string[]).includes(c)),
      models: Array.isArray(p.models) ? p.models.filter((m): m is OwnModel => Boolean(m) && typeof m.id === 'string').map((m) => {
        const reasoning = parseModelReasoning(m.reasoning)
        return { id: m.id, name: typeof m.name === 'string' && m.name ? m.name : m.id, vision: m.vision === true, kind: m.kind === 'image' || m.kind === 'video' ? m.kind : 'chat', ...(reasoning ? { reasoning } : {}) }
      }) : [],
      at: Number(p.at) || 0,
      // kept, or the next start's refresh would write the row as a hand-declared route
      ...(p.catalogRoute === true ? { catalogRoute: true } : {}),
      ...(p.customModels === true ? { customModels: true } : {}),
    }
  }
  return out
}

/** The `confirmed` field of a hands call, when it is one (a ticket or the legacy `true`). */
export function confirmationOf(args: unknown): string | true | undefined {
  if (!args || typeof args !== 'object' || Array.isArray(args)) return undefined
  const value = (args as Record<string, unknown>).confirmed
  return value === true || typeof value === 'string' ? value : undefined
}

/** The confirmation ticket `nanomuse mcp` checks: HMAC-SHA256 over the compact, key-sorted arguments without `confirmed`. */
export function confirmTicket(secret: string, args: Record<string, unknown>): string {
  const clean = Object.fromEntries(Object.entries(args).filter(([k]) => k !== 'confirmed'))
  return createHmac('sha256', secret).update(canonical(clean)).digest('hex').slice(0, 32)
}

function canonical(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonical).join(',')}]`
  if (value && typeof value === 'object') {
    const entries = Object.entries(value as Record<string, unknown>).filter(([, v]) => v !== undefined).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
    return `{${entries.map(([k, v]) => `${JSON.stringify(k)}:${canonical(v)}`).join(',')}}`
  }
  return JSON.stringify(value)
}

/**
 * The runtime's refusal in an MCP error result, or `undefined` for any other failure.
 * The mark it is recognised by: `nanomuse mcp` said "Not done — " until 1.0 and has said
 * "Not done: " since (the runtime's sentences carry no dashes, `tests/test_voice.py`); this
 * plugin's own declines say "Not done: " too. Both marks are the same refusal, and the old
 * one stays recognised for a runtime that predates 1.0.
 */
const REFUSAL_MARKS = ['Not done: ', 'Not done — '] as const

export function refusalOf(result: ToolExecutionResult): string | undefined {
  const texts: string[] = []
  for (const block of result.content ?? []) {
    const b = block as { type?: unknown; text?: unknown }
    if (b.type === 'text' && typeof b.text === 'string') texts.push(b.text)
  }
  const message = (result.error as { message?: unknown } | undefined)?.message
  if (typeof message === 'string') texts.push(message)
  for (const text of texts) {
    const hit = REFUSAL_MARKS.map((mark) => ({ mark, at: text.indexOf(mark) }))
      .filter(({ at }) => at >= 0)
      .sort((a, b) => a.at - b.at)[0]
    if (hit) return text.slice(hit.at + hit.mark.length).trim()
  }
  return undefined
}

/** Every text of a tool result's error, joined: what the runtime said went wrong. */
function errorText(result: ToolExecutionResult): string {
  const texts: string[] = []
  for (const block of result.content ?? []) {
    const b = block as { type?: unknown; text?: unknown }
    if (b.type === 'text' && typeof b.text === 'string') texts.push(b.text)
  }
  const m = (result.error as { message?: unknown } | undefined)?.message
  if (typeof m === 'string') texts.push(m)
  return texts.join('\n')
}

/** The plugin's own refusal of a step the person declined on the card: the same mark as the runtime's. */
export function declined(step: string, why: string): ToolExecutionResult {
  const text = `Not done: ${step}. The person did not approve this step on their permission card (${why}); do not retry it. Ask them what to do instead, or carry on without it.`
  return { isError: true, error: { message: text, info: { name: 'HandsDeclined', code: 'REJECTED' } }, content: [{ type: 'text', text }] }
}

const ARG_KEYS = ['action', 'label', 'text', 'device', 'command', 'task', 'path', 'url'] as const

export default class NanomuseCloud extends Service {
  static inject = ['credentials', 'settings']
  static Config = Config

  readonly relay: Relay
  readonly profile: ProfileStore
  readonly hub: HubClient
  private mordeHub: HubClient | undefined
  private mordeHubHasCredential = false
  private mordeHubCapabilities = { screenshot: false, files: false, command: false }
  private mordeHubDevices: MordeHubDevice[] = []
  private mordeHubDeviceError: string | undefined
  private state: State = {}
  private busy: Promise<unknown> = Promise.resolve()
  private readonly streams = new Set<ServerResponse>()
  private readonly changeListeners = new Set<() => void>()
  private readonly calls = new Map<string, HandsCall>()
  /** Sessions sent through the account for one turn (*Use nanoMuse Cloud this time*), with what to put back. */
  private readonly cloudOnce = new Map<string, CloudOnce>()
  /** The scoped context with the session API, once it is up. */
  private sessionCtx: Context | undefined
  private steps = 0
  private lastCallAt = 0
  private notices: Notice[] = []
  /** Questions from other devices waiting on this screen. */
  private readonly asks = new AskDesk(() => this.broadcast())
  private noticeSeq = 0
  private stage: StageState = NO_STAGE
  /** The hands' runs, step by step, with their pictures (0.1.40). */
  private readonly trajectory = new Trajectory()
  /** Which session has this computer's screen right now, for the glow and the capsule. */
  private readonly screenRuns = new ScreenRuns()
  /** The turns open on every session, for the exit's aggregate (src/work.ts). */
  private readonly turns = new Turns()
  /** The exit's gate: closed while the desktop shell hands over, new tasks refused and turns that open are stopped at once. */
  private gateClosed = false
  private runTimer: NodeJS.Timeout | undefined
  private stageTimer: NodeJS.Timeout | undefined
  private signedInCache = false
  /** When the account was last re-read for the allowance (after a turn, after a refusal). */
  private lastAllowanceCheck = 0
  /** Tasks from other devices, once the session API is up. */
  private tasks: TaskRunner | undefined
  /** Conversations synced between the account's devices (contract C7), once the session API is up. */
  private sync: SyncEngine | undefined
  /** The other devices' turns kept for the transcript (C8), by session. */
  private kept: RemoteStore | undefined
  /**
   * What the sync needs of a session's log, read once per change: a session's log is read
   * in full (and decompressed) by `inspect`, so the title, the lines and the prompts'
   * positions are kept here until the session's next event (`session/event`) drops them.
   */
  private readonly logCache = new Map<string, { title?: { at: number; title: string }; lines?: SessionLine[]; prompts?: Array<{ key: string; time: number }> }>()
  /** Approvals the stage may answer (C2) and holds of the hands (C1). */
  /**
   * The scheduled tasks whose reminder opened a session's current turn (dsh-schedule posts it as a
   * user message from `schedule`); gone at the turn's end or when the person writes in that turn.
   */
  private readonly scheduledTurns = new Map<string, string[]>()
  /** The tasks of task grants that were deleted since, and the tasks in the chats of chat grants (`refreshGrantTasks`). */
  private goneSchedules: string[] = []
  private chatTasks: Record<string, string[]> = {}
  private readonly approvalDesk = new ApprovalDesk(
    () => this.deskMoved(),
    (req) => this.granted(req.toolName, String(req.agent.session.id)),
    (req) => this.handsApp(req.toolName),
    (req) => this.scheduleAsking(req.toolName, String(req.agent.session.id)),
  )
  private readonly holdDesk = new HoldDesk(() => this.deskMoved())
  /** Sessions the other devices were last told are waiting on a card or a hold here (0.31). */
  private waitingSessions = new Set<string>()
  private updateCheck: Promise<UpdateInfo> | undefined
  /** The hourly retry after a check that read nothing; one at a time, dropped at shutdown. */
  private updateRetry: ReturnType<typeof setTimeout> | undefined
  private lastSharedConnectors = ''
  /** The face's clips (desk-b). */
  readonly motion: AvatarMotion
  /** The person's own clip as the face (#324), this computer's alone. */
  readonly ownClip: OwnClip
  /** Turns run on a key of the person's own, sent up to the account's record afterwards (relay 0.29). */
  readonly ownTurns: OwnTurns
  private blackScreenAt = 0
  private handsMount: HandsMount = { at: 0, ok: true, reason: '' }
  /** The own-key catalogue (C11), from `assets/providers.json`. */
  private catalogue: ProviderEntry[] = []
  /** The ChatGPT sign-in and its proxy (C11), over the runtime's `nanomuse chatgpt`. */
  readonly chatgpt: ChatGptDesk
  /** The Cursor, Codex, Claude Code and DeepSeek Harness sessions on this computer, for the page and the other devices (`docs/coding-agents.md`). */
  readonly coding: CodingService

  constructor(ctx: Context, private readonly config: Config) {
    super(ctx, 'nanomuseCloud')
    this.relay = new Relay(config.baseURL)
    this.chatgpt = new ChatGptDesk({
      command: async () => {
        const info = await runtimeInfo()
        return info.ok ? info.path : undefined
      },
      onReady: (url, token, models) => this.serialize(() => this.chatGptReady(url, token, models)),
      onSignedOut: () => void this.serialize(() => this.chatGptGone()).catch((error: unknown) => this.ctx.logger.warn('nanomuse: chatgpt row not removed: %s', message(error))),
      onChange: () => this.broadcast(),
      log: (level, text) => this.ctx.logger[level](text),
    })
    this.profile = new ProfileStore(this.dir(), this.relay)
    this.motion = new AvatarMotion({
      dir: join(this.dir(), 'avatar', 'motion'),
      endpoint: (viaCloud) => this.videoEndpoint({ cloud: viaCloud === true }),
      animate: () => this.state.media?.animate !== false,
      faceId: () => this.faceId(),
      still: (mood) => this.faceStill(mood),
      onChange: () => this.broadcast(),
      log: (level, text) => this.ctx.logger[level](text),
      share: (mood, bytes) => this.shareClip(mood, bytes),
    })
    this.ownClip = new OwnClip({
      dir: join(this.dir(), 'avatar'),
      onChange: () => this.broadcast(),
    })
    this.ownTurns = new OwnTurns({
      enabled: () => this.recordOwnTurns(),
      readImage: (ref) => this.ctx.attachments.readImage(ref),
      hasBlob: async (sha256) => this.relay.hasBlob((await this.token()) ?? '', sha256),
      putBlob: async (file) => this.relay.putBlob((await this.token()) ?? '', file.sha256, file.mime, file.data),
      postTurn: async (turn) => {
        const token = await this.token()
        if (!token) return
        try {
          await this.relay.postTurn(token, turn as unknown as Record<string, unknown>, this.state.deviceId ?? '')
        } catch (error: unknown) {
          // a relay that keeps no record (RECORD_TURNS=0, or an older one): stop sending until the account is read again
          if (error instanceof RelayError && error.status === 404) this.recordOff = true
          throw error
        }
      },
      providerName: (route) => this.state.providers?.[route]?.provider ?? route,
      log: (level, text) => this.ctx.logger[level](text),
    })
    this.hub = new HubClient({
      url: this.relay.hubURL,
      key: () => this.token(),
      device: () => {
        const llm = this.helloLlm()
        return {
          id: this.state.deviceId ?? '',
          name: this.deviceName(),
          kind: 'computer',
          os: `${type()} ${release()}`.trim(),
          version: VERSION,
          actions: this.actions(),
          ...(llm ? { llm } : {}),
        }
      },
      log: (level, text) => this.ctx.logger[level](text),
    })
    this.coding = new CodingService({
      storePath: join(this.dir(), 'coding', 'runs.json'),
      hub: this.hub,
      log: (level, text) => this.ctx.logger[level](text),
    })
  }

  async [Service.init](): Promise<void> {
    this.state = await this.readState()
    if (!this.state.deviceId) {
      this.state.deviceId = `pc-dsh-${randomBytes(6).toString('hex')}`
      await this.writeState()
    }
    this.catalogue = await loadCatalogue()
    if (!this.catalogue.length) this.ctx.logger.warn('nanomuse: assets/providers.json missing or empty; the own-key step lists nothing')
    await this.cleanupOwnProviderKeys()
    // Keys saved before the catalogue learned a vendor's thinking levels get them on this start.
    this.refreshOwnRows()
    this.ctx.effect(
      () =>
        this.ctx.on('settings/document-updated', (ns) => {
          if (String(ns) === LLM_ROW && this.reconcileOwnRows()) this.broadcast()
        }),
      'nanomuse cloud: own-key catalog edits',
    )
    this.ctx.effect(
      () =>
        this.ctx.on('llm/adapters-updated', () => {
          if (this.reconcileOwnRows()) this.broadcast()
        }),
      'nanomuse cloud: adapter catalog edits',
    )
    await this.profile.load()
    this.profile.onChange(() => this.broadcast())
    // The face's clips follow the face (C3): a face drawn here or pulled from the account drops the
    // old clips and, with the Media setting on and a video model at hand, is animated again.
    await this.motion.init().catch((error: unknown) => this.ctx.logger.warn('nanomuse: avatar clips not read: %s', message(error)))
    await this.ownClip.init().catch((error: unknown) => this.ctx.logger.warn('nanomuse: own clip not read: %s', message(error)))
    this.profile.onChange(() => void this.animateNewFace().catch((error: unknown) => this.ctx.logger.warn('nanomuse: avatar motion: %s', message(error))))
    this.hub.onState(() => this.broadcast())
    this.hub.onDevices(() => this.broadcast())
    this.hub.onProfile(() => void this.pullProfile().catch((error: unknown) => this.ctx.logger.warn('nanomuse: profile pull failed: %s', message(error))))
    this.hub.onUnauthorized(() => void this.serialize(() => this.forget()).catch(() => undefined))
    this.hub.handle('info', async () => ({
      name: this.deviceName(),
      kind: 'computer',
      os: type(),
      os_version: release(),
      arch: arch(),
      user: userInfo().username,
      home: homedir(),
      cwd: process.cwd(),
      runtime: VERSION,
      actions: this.actions(),
    }))
    this.hub.handle('notify', async (args, call) => {
      this.notice('notify', call.from.name || 'a device', String(args.title ?? ''), String(args.text ?? ''))
      return { ok: true, shown: true }
    })
    for (const action of REMOTE_ACTIONS) {
      this.hub.handle(action, (args, call) => this.remote(action, args, call.from))
    }
    // The coding agents on this computer, for the other devices (`docs/coding-agents.md`): the
    // lists are read-only and answer at once; a message into an agent edits files here, so it
    // is agreed to like a `shell` command — the card on this screen unless remote control is on.
    for (const action of CODING_ACTIONS) {
      this.hub.handle(action, async (args, call) => {
        if (GATED_CODING_ACTIONS.has(action)) await this.permit(call.from, action, codingBrief(action, args))
        return this.coding.handle(action, args, call)
      })
    }
    void this.coding.load()
    // A task from another device runs in a dsh session here; needs the session API, so only once it is up.
    this.ctx.inject(['sessionController', 'approval'], (ctx) => {
      this.sessionCtx = ctx
      ctx.effect(() => () => {
        this.sessionCtx = undefined
      })
      // The stage answers approvals too; registered before the task runner so a task from another
      // device (answered on that device) never reaches this desk.
      ctx.effect(() => ctx.on('approval/request', (req, next) => this.approvalDesk.handle(req, next), true), 'nanomuse cloud: approvals on the stage')
      // a task grant whose task was deleted is listed as such, for revoking
      void this.refreshGrantTasks()
      ctx.effect(() => ctx.on('schedule/changed' as never, (() => void this.refreshGrantTasks()) as never), 'nanomuse cloud: the tasks behind the grants')
      const runner = new TaskRunner(ctx, {
        deviceName: () => this.deviceName(),
        log: (level, text) => this.ctx.logger[level](text),
        notice: (from, text) => this.notice('call', from, '', text, 'task'),
        recall: (key) => this.state.taskSessions?.[key],
        remember: (key, sessionId) => {
          this.state.taskSessions = { ...this.state.taskSessions, [key]: sessionId }
          void this.writeState().catch((error: unknown) => this.ctx.logger.warn('nanomuse: state write failed: %s', message(error)))
        },
        closed: () => this.gateClosed,
      })
      this.tasks = runner
      ctx.effect(() => runner.attach(), 'nanomuse cloud: tasks')
      ctx.effect(
        () =>
          this.hub.handle('task', async (args, call) => {
            await this.permit(call.from, 'task', brief('task', args))
            return runner.task(args, call)
          }),
        'nanomuse cloud: task',
      )
      ctx.effect(() => this.hub.handle('stop', (args, call) => runner.stop(args, call)), 'nanomuse cloud: stop')
      ctx.effect(() => this.hub.handle('approve', (args, call) => runner.approve(args, call)), 'nanomuse cloud: approve')
      ctx.effect(() => () => {
        this.tasks = undefined
      }, 'nanomuse cloud: tasks off')
      // The account's conversations, the same on every device (C7, C8): the person's words go up as
      // they are sent and the model's when the turn ends; the other devices' conversations are chats
      // here, their turns kept in the host's own store for the transcript and handed to the model as context.
      const kept = new RemoteStore(join(dshHome(), 'nanomuse', 'sync-remote.json'), (error) => this.ctx.logger.warn('nanomuse cloud: remote turns not saved: %s', message(error)))
      this.kept = kept
      const sync = new SyncEngine({
        relay: new SyncRelay(this.config.baseURL),
        sessions: {
          list: async () => {
            const { items } = await ctx.sessionController.list({}, new AbortController().signal)
            // The title comes from the list's own `title` projection when the host has one; only a
            // session without it (no title landed yet) is read, and that once per change: reading
            // every log on every push kept the host busy for seconds on a big account.
            const rows: SessionInfo[] = []
            for (const s of items) {
              if (s.parentSessionId || s.origin === 'subagent') continue
              const id = String(s.sessionId)
              const hint = s.projections?.values?.title
              const title = typeof hint === 'string' && hint ? hint : await this.sessionTitle(ctx, s.sessionId, s.updatedAt)
              rows.push({ id, title, blank: s.blank, createdAt: s.updatedAt, updatedAt: s.updatedAt })
            }
            return rows
          },
          lines: (sessionId) => this.sessionLines(ctx, sessionId),
          create: async (title) => {
            // In the home workspace (`~/nanoMuse`), as the first conversation is: a session the
            // browser cannot place in a workspace shows a disabled composer and no transcript.
            const folder = join(homedir(), 'nanoMuse')
            await mkdir(folder, { recursive: true }).catch(() => undefined)
            const registry = ctx.get('workspaceRegistry') as { create(path: string, title?: string): Promise<{ id: string }> } | undefined
            const workspaceId = await registry?.create(folder, 'nanoMuse').then((w) => w.id).catch(() => undefined)
            const created = await ctx.sessionController.create(
              workspaceId ? { agentPreset: 'nanomuse', workspaceId: workspaceId as NonNullable<SessionCreateRequest['workspaceId']> } : { agentPreset: 'nanomuse', cwd: folder },
            )
            await ctx.sessionController.rename({ sessionId: created.sessionId, title }).catch(() => undefined)
            return String(created.sessionId)
          },
          rename: async (sessionId, title) => {
            await ctx.sessionController.rename({ sessionId: sessionId as SessionId, title })
          },
          keep: async (sessionId, line) => kept.add(sessionId, line),
          forget: async (sessionId, mid) => kept.remove(sessionId, mid),
          // another account signed in (C10): the last account's turns from elsewhere go; its mapping stays
          forgetAll: async () => {
            kept.clear()
            this.logCache.clear()
          },
          inject: async (sessionId, text) => {
            const resolved = await ctx.sessionController.resolveAgent(sessionId as SessionId)
            if ('error' in resolved) throw new Error(String(resolved.error))
            // model-facing: a `nanomuse-sync` source, so the push (`sessionLines`, `user` sources only) never sends it back
            resolved.agent.inject(createUserMessage({ content: [{ type: 'text', text }], source: { kind: 'nanomuse-sync', role: 'user', mid: '', device: '', deviceName: '', at: Date.now() } }))
          },
        },
        token: () => this.token(),
        deviceId: () => this.state.deviceId ?? '',
        isTaskSession: (sessionId) => Object.values(this.state.taskSessions ?? {}).includes(sessionId),
        load: () => this.state.sync,
        save: async (state) => {
          this.state.sync = state
          await this.writeState()
        },
        onChange: () => this.broadcast(),
        log: (level, text) => this.ctx.logger[level](text),
      })
      this.sync = sync
      ctx.effect(
        () =>
          ctx.on('session/event', (session: Session, event: SessionEvent) => {
            // `session/title` is dsh-session-title's event (not a dependency here): matched by name
            const id = String(session.id)
            this.logChanged(id)
            if (event.type === 'turn/end' && this.cloudOnce.has(id)) void this.cloudOnceDone(id)
            if (event.type === 'turn/end') sync.turnEnded(id)
            else if (event.type === 'user/message' && (event.data as { source?: { kind?: string } }).source?.kind === 'user') sync.messageSent(id)
            // the run's steps, for the other devices' working line (0.31): a tool call in its own words, the model writing
            else if (event.type === 'tool/call') sync.step(id, 'working', describeCall(event.data.name, event.data.arguments))
            else if (event.type === 'assistant/message') sync.step(id, 'working', '')
            else if ((event as { type: string }).type === 'session/title') sync.sessionRenamed(String(session.id))
          }),
        'nanomuse cloud: sync turns',
      )
      ctx.effect(() => this.hub.onSync((cursor, from) => sync.onFrame({ cursor, from })), 'nanomuse cloud: sync frame')
      ctx.effect(() => this.hub.onWorking((frame) => sync.onWorking(frame)), 'nanomuse cloud: working frame')
      ctx.effect(
        () =>
          this.hub.onState(() => {
            if (this.hub.connected) sync.onFrame({})
          }),
        'nanomuse cloud: sync on hub connect',
      )
      if (this.state.account) sync.start()
      ctx.effect(() => () => {
        sync.stop()
        this.sync = undefined
        this.kept = undefined
        void kept.flush()
      }, 'nanomuse cloud: sync off')
      if (this.hub.connected) this.hub.restart()
    })

    mountGuarded(this.ctx, API_PREFIX, this.handle, 'nanomuse cloud: api')
    // The account's other devices see what is connected here (C3): on every change and at start.
    this.ctx.inject(['nanomuseConnectors'], (ctx) => {
      ctx.effect(() => ctx.nanomuseConnectors.onChange(() => void this.publishConnectors().catch((error: unknown) => this.ctx.logger.warn('nanomuse cloud: connectors not shared: %s', message(error)))), 'nanomuse cloud: share connectors')
      void this.publishConnectors().catch((error: unknown) => this.ctx.logger.warn('nanomuse cloud: connectors not shared: %s', message(error)))
    })
    this.ctx.inject(['tools'], (ctx) => {
      ctx.on('tools/execute', async (exec, next) => {
        if (!isHandsTool(exec.name)) return next()
        // Our own re-dispatch of a step the person just agreed to: the stage already follows the outer call.
        if (exec.parent !== undefined && confirmationOf(exec.arguments) !== undefined) return next()
        const sessionId = exec.agent?.session.id ?? ''
        // The agent hands the screen to the person (C1): a hold until they say Done, then a fresh look.
        if (exec.name === 'mcp__nanomuse__computer_act' && handOverOf(exec.arguments)) {
          const reason = handOverOf(exec.arguments) ?? ''
          this.began(exec.callId, exec.name, exec.arguments, sessionId)
          this.acted({ kind: 'hold', label: reason, text: reason, x: -1, y: -1, x2: -1, y2: -1, dy: 0, at: Date.now() }, sessionId, exec.callId)
          try {
            const hold = this.holdDesk.begin(sessionId, 'computer', 'agent', reason)
            const how = await this.holdDesk.wait(sessionId)
            if (how !== 'done') this.holdDesk.done(hold.id)
            const text = how === 'done' ? 'The person finished and handed the screen back.' : how === 'timeout' ? 'Nobody came back within ten minutes; the screen is yours again.' : 'The hand-over was cancelled.'
            return { isError: false, value: text, content: [{ type: 'text', text }] } as Awaited<ReturnType<typeof next>>
          } finally {
            this.ended(exec.callId)
          }
        }
        // While the person has the screen, the hands wait rather than fail.
        if (this.holdDesk.on(sessionId)) await this.holdDesk.wait(sessionId)
        this.began(exec.callId, exec.name, exec.arguments, sessionId)
        if (exec.name === 'mcp__nanomuse__computer_act') this.acted(stageAction(exec.arguments), sessionId, exec.callId)
        else if (exec.name === 'mcp__nanomuse__computer_screen') this.acted({ kind: 'look', label: '', text: '', x: -1, y: -1, x2: -1, y2: -1, dy: 0, at: Date.now() }, sessionId, exec.callId)
        try {
          let result = await next()
          if (exec.name === 'mcp__nanomuse__computer_act' && result.isError) {
            const refused = refusalOf(result)
            if (refused) result = await this.confirmStep(ctx, exec, refused, next)
          }
          if (exec.name.startsWith('mcp__nanomuse__computer_') && !result.isError) this.frameFromMcp(result.value, sessionId, exec.callId)
          // A black capture (macOS: Screen Recording missing for the app, or granted after it started) puts a relaunch notice up.
          if (exec.name.startsWith('mcp__nanomuse__computer_') && result.isError && isBlack(errorText(result))) this.sawBlackScreen()
          return result
        } finally {
          this.ended(exec.callId)
        }
      })
    })
    // The trajectory (0.1.40): the model's words before a step, and the turn's end closing the
    // run; the same end lets go of the screen (the glow and the capsule outside the window).
    this.ctx.effect(
      () =>
        this.ctx.on('session/event', (session: Session, event: SessionEvent) => {
          const id = String(session.id)
          if (event.type === 'assistant/message') this.trajectory.said(id, textOf(event.data.message.content))
          else if (event.type === 'turn/end') {
            let changed = this.screenRuns.turnEnded(id)
            if (this.trajectory.running(id)) {
              this.trajectory.turnEnded(id)
              changed = true
            }
            if (changed) {
              this.armRunSweep()
              this.broadcast()
            }
          }
        }),
      'nanomuse cloud: trajectory',
    )
    // The turns of every session, for the desktop shell's protected exit (src/work.ts): a chat
    // reply, a goal's automation or a task another device asked for is a turn here. While the
    // exit hands over (the gate closed), a turn that opens all the same is stopped at once;
    // nothing runs under the hand-over, and what the asker sent comes back as refused.
    this.ctx.effect(
      () =>
        this.ctx.on('session/event', (session: Session, event: SessionEvent) => {
          const id = String(session.id)
          if (event.type === 'turn/start') {
            this.turns.began(id)
            if (this.gateClosed) void this.abortSession(id, { kind: 'hook', reason: 'this computer is ending its work; nothing starts under the hand-over' })
          } else if (event.type === 'turn/end') {
            this.turns.ended(id)
            this.scheduledTurns.delete(id)
          } else if (event.type === 'user/message') {
            const kind: string = event.data.source?.kind ?? ''
            if (kind === 'schedule') this.scheduledTurns.set(id, scheduleRunIds(textOf(event.data.content)))
            else if (!HARNESS_CONTEXT_KINDS.has(kind)) this.scheduledTurns.delete(id)
          }
        }),
      'nanomuse cloud: turns for the protected exit',
    )
    // The relay's refusals, read on their way back (C12): a spent allowance, a request too large,
    // a retired key, a relay that did not answer — as a card in the chat, not the wire's text,
    // and not retried as if they were rate limits.
    // The same hook is the one place that keeps a switched-off Cloud from spending (Settings ›
    // Models › *Use nanoMuse Cloud models*): a side call of the harness (a chat's title, a
    // compaction) is sent to the chat slot's own model instead, a turn is refused with a card,
    // and only a session under *Use nanoMuse Cloud this time* (`cloudOnce`) goes through.
    const cloud = this
    this.ctx.inject(['llm'], (ctx) => {
      ctx.on('llm/stream', function (options, next) {
        // a key of the person's own: the turn is let through as it is and, when the account is signed
        // in and the relay keeps a record, sent up afterwards so the record is whole (docs/cloud.md)
        if (options.provider !== PROVIDER_ID) return cloud.recordOwnTurns() ? cloud.ownTurns.watch(options, next()) : next()
        if (!cloud.cloudModels() && !(options.sessionId && cloud.cloudOnce.has(String(options.sessionId)))) {
          const own = options.purpose ? cloud.ownChatChoice() : undefined
          if (own) {
            cloud.ctx.logger.info('nanomuse cloud: %s sent to %s/%s, Cloud models are off', options.purpose, own.provider, own.model)
            return this.stream({ ...options, provider: own.provider, model: own.model })
          }
          cloud.ctx.logger.info('nanomuse cloud: a call to %s refused, Cloud models are off%s', options.model, options.purpose ? ` (${options.purpose})` : '')
          return cloud.refuseCloudOff()
        }
        return cloud.watchRelayStream(next(), options.sessionId === undefined ? '' : String(options.sessionId))
      })
    })
    this.ctx.effect(() => () => {
      this.hub.stop('shutting down')
      this.mordeHub?.stop('shutting down')
      this.coding.close()
      this.chatgpt.stop()
      if (this.updateRetry) clearTimeout(this.updateRetry)
      this.updateRetry = undefined
      for (const res of this.streams) res.end()
      this.streams.clear()
    }, 'nanomuse cloud: hub')
    // A ChatGPT sign-in on file (C11): the proxy comes up for the host's lifetime, once the runtime confirms the tokens are still there.
    if (this.state.chatgpt) {
      void this.chatgpt.status().then((status) => {
        if (!status) return // no runtime right now: the row stays, the proxy waits for the next start
        if (status.signedIn) this.chatgpt.start()
        else return this.serialize(() => this.chatGptGone())
        return undefined
      }).catch((error: unknown) => this.ctx.logger.warn('nanomuse: chatgpt status: %s', message(error)))
    }

    if (this.state.account && (await this.token())) {
      this.signedInCache = true
      this.hub.start()
      // Refresh what the account looks like, quietly; the key may have been retired elsewhere.
      void this.refresh().catch((error: unknown) => {
        this.ctx.logger.warn('nanomuse cloud: could not refresh the account: %s', message(error))
      })
    }
    this.mordeHubHasCredential = Boolean(await this.credential(MORDE_CLIENT_CREDENTIAL))
    if (this.mordeHubHasCredential && this.state.mordeHub?.installationID) this.startMordeHub()
    // Once a day, quietly: a newer release puts a badge on Settings and a row in the ••• menu.
    // A check that read nothing (the network down at start) counts for an hour, not a day.
    if (!updateCacheFresh(this.state.update, BUNDLE_VERSION)) {
      setTimeout(() => void this.update(false).catch(() => undefined), 15_000).unref?.()
    }
  }

  // ---- the update check (B5) -------------------------------------------------------

  /**
   * The newest release against what runs here; cached a day unless `force`, an hour when
   * the last check read nothing. The About row's own button is `force`: a failed check in
   * the cache never stands in its way.
   */
  update(force: boolean): Promise<UpdateInfo> {
    const cached = this.state.update
    // a cache made by another version of this bundle (the one that ran before an update was
    // installed) compared the wrong `current`; it is not an answer for this one (#341)
    if (!force && cached && updateCacheFresh(cached, BUNDLE_VERSION)) return Promise.resolve(cached)
    if (this.updateCheck) return this.updateCheck
    // the bare number: 0.1.40 compared the labelled `VERSION` ("dsh-nanomuse 0.1.40"), which
    // parsed as 0, so every release read as newer and the installed one was offered again
    this.updateCheck = checkForUpdate(BUNDLE_VERSION)
      .then(async (info) => {
        this.state = { ...this.state, update: info }
        await this.writeState().catch(() => undefined)
        this.broadcast()
        this.scheduleUpdateRetry(info)
        return info
      })
      .finally(() => {
        this.updateCheck = undefined
      })
    return this.updateCheck
  }

  /** After a check that read neither source: one quiet try an hour later, replacing any retry still waiting. */
  private scheduleUpdateRetry(info: UpdateInfo): void {
    if (this.updateRetry) clearTimeout(this.updateRetry)
    this.updateRetry = undefined
    if (info.source !== 'none') return
    this.updateRetry = setTimeout(() => {
      this.updateRetry = undefined
      void this.update(false).catch(() => undefined)
    }, UPDATE_RETRY_MS)
    this.updateRetry.unref?.()
  }

  // ---- the relay's refusals in the chat (C12) ---------------------------------------

  /**
   * The account's stream, watched: a finish that is a refusal of the relay's is rewritten
   * (`src/refusals.ts`) so the chat draws the card and the retry plugin lets it be; a spent
   * allowance has the account re-read, so Settings and the heads-up agree with it; a 401 has
   * the sign-in forgotten here, as a `/me` 401 does. A turn that ended well counts for the
   * 80 % heads-up. Until the first chunk the call is in `waits`, so the chat can say how long
   * it has been waiting (0.31; the relay's own "queued, N ahead" chunks have no choices and
   * never reach this side, so the line counts the seconds and nothing more).
   */
  private async *watchRelayStream(stream: AsyncIterable<StreamChunk>, session = ''): AsyncIterable<StreamChunk> {
    const id = ++this.waitSeq
    this.waits.set(id, { since: Date.now(), session })
    this.broadcast()
    try {
      for await (const chunk of stream) {
        if (this.waits.delete(id)) this.broadcast()
        if (chunk.type !== 'finish') {
          yield chunk
          continue
        }
        if (chunk.reason.kind !== 'error') {
          if (chunk.reason.kind !== 'aborted') this.afterRelayTurn()
          yield chunk
          continue
        }
        const seen = relayFailure(chunk.reason.failure)
        if (!seen) {
          yield chunk
          continue
        }
        this.ctx.logger.info('nanomuse cloud: the relay refused (%s %s): %s', seen.refusal.status, seen.refusal.code, seen.refusal.message || '—')
        this.refused(seen.refusal)
        yield { ...chunk, reason: { kind: 'error', failure: { ...chunk.reason.failure, ...seen.failure } } }
      }
    } finally {
      if (this.waits.delete(id)) this.broadcast()
    }
  }

  /** The account's model calls still without a first token, by a number of their own (0.31). */
  private readonly waits = new Map<number, { since: number; session: string }>()
  private waitSeq = 0

  /** Set when the relay answered that it keeps no record; cleared when the account is read again. */
  private recordOff = false

  /**
   * Whether a turn run on a key of the person's own goes up to the account's record: signed in,
   * the relay said it keeps one (`contribute.record.on`, relay 0.29), and it has not refused
   * since. Nothing of the kind on an older relay or when signed out.
   */
  recordOwnTurns(): boolean {
    if (!this.signedInCache || !this.state.account || this.recordOff) return false
    return this.state.account.contribute?.record?.on === true
  }

  /**
   * Whether nanoMuse Cloud is one of the model sources: signed in and not switched off under
   * Settings › Models. Every slot's order and list, the side calls and the studio read this one
   * answer; the sign-in itself (`signedIn`) is a different question, and stays.
   */
  cloudModels(): boolean {
    return this.signedInCache && Boolean(this.state.account) && !this.state.cloudModelsOff
  }

  /**
   * Switch the account's models on or off as a source (the phones' *Use nanoMuse Cloud models*).
   * Off while new chats answer through the account: the chat slot moves to the first own chat
   * model, when there is one; the hands' environment follows. With no own chat model the
   * switch is refused (`409 chat_on_cloud`, the same rule on every client): nothing else
   * could answer, so the state does not change and the switch stays on.
   */
  async setCloudModels(on: boolean): Promise<void> {
    if (on === !this.state.cloudModelsOff) return
    const own = !on && this.chatChoice().provider === PROVIDER_ID ? this.ownChatChoice() : undefined
    if (!on && this.chatChoice().provider === PROVIDER_ID && !own) throw new RelayError(409, 'chat_on_cloud', CHAT_ON_CLOUD_MESSAGE)
    const { cloudModelsOff: _off, ...rest } = this.state
    this.state = on ? rest : { ...rest, cloudModelsOff: true }
    if (own) await this.defaultModelService()?.saveSelection(own).catch((error: unknown) => this.ctx.logger.warn('nanomuse cloud: chat slot not moved off the account: %s', message(error)))
    await this.writeState()
    await this.writeHands().catch((error: unknown) => this.ctx.logger.warn('nanomuse cloud: hands model not written: %s', message(error)))
    this.broadcast()
  }

  /** The own chat model a side call runs on while Cloud models are off: the chat slot's own row (its catalogue default), else the first own row with a chat model. */
  private ownChatChoice(): { provider: string; model: string } | undefined {
    const chat = this.chatChoice()
    const row = chat.provider && chat.provider !== PROVIDER_ID ? this.state.providers?.[chat.provider] : undefined
    if (row && chat.model && row.models.some((m) => m.id === chat.model && m.kind === 'chat')) return { provider: chat.provider, model: chat.model }
    if (row) {
      const model = this.ownModelFor(row, 'chat')
      if (model) return { provider: chat.provider, model }
    }
    for (const [id, own] of Object.entries(this.state.providers ?? {})) {
      const model = this.ownModelFor(own, 'chat')
      if (model) return { provider: id, model }
    }
    return undefined
  }

  /** The stream for a call refused because Cloud models are off: one finish chunk, our code, so the chat shows the card and the harness does not retry. */
  private async *refuseCloudOff(): AsyncIterable<StreamChunk> {
    yield { type: 'finish', reason: { kind: 'error', failure: cloudOffFailure() } }
  }

  /** What a refusal changes here, besides the card: the account re-read, or forgotten. */
  private refused(refusal: RelayRefusal): void {
    if (refusal.kind === 'exhausted' || refusal.kind === 'allowance_paused' || refusal.kind === 'daily_cap') {
      this.lastAllowanceCheck = Date.now()
      void this.refresh().catch((error: unknown) => this.ctx.logger.warn('nanomuse cloud: account not re-read after the refusal: %s', message(error)))
    } else if (refusal.kind === 'signed_out') {
      void this.serialize(() => this.forget()).then(() => this.broadcast()).catch((error: unknown) => this.ctx.logger.warn('nanomuse cloud: sign-in not forgotten after a 401: %s', message(error)))
    }
  }

  /**
   * After a turn on the account's model: once a minute at most, re-read the account, and when
   * the relay says `warn` (80 % of the pool spent) show the heads-up strip — once per pool size
   * (the phones' `nmAllowanceHeadsUp`, the web app's `AllowanceHeadsUp`).
   */
  private afterRelayTurn(): void {
    if (!this.state.account || Date.now() - this.lastAllowanceCheck < ALLOWANCE_CHECK_EVERY_MS) return
    this.lastAllowanceCheck = Date.now()
    void this.serialize(async () => {
      const token = await this.token()
      if (!token || !this.state.account) return
      const account = await this.relay.me(token)
      this.state = { ...this.state, account }
      this.recordOff = false
      await this.writeState()
    }).then(() => this.broadcast()).catch((error: unknown) => this.ctx.logger.debug('nanomuse cloud: allowance not re-read: %s', message(error)))
  }

  /** The 80 % heads-up, when it is due: what is left, the pool, and what an invitation adds. */
  private headsUp(): LiveState['headsUp'] {
    const spend = this.state.account?.spend
    if (!spend || spend.unlimited || !spend.warn) return null
    const left = spend.left ?? 0
    const grant = spend.grant ?? 0
    if (left <= 0 || grant <= 0 || this.state.warnedGrant === grant) return null
    return { left, grant, bonus: spend.inviteBonusCny ?? 5 }
  }

  /** The person saw the heads-up (or followed it): not again for this pool size. */
  async headsUpSeen(): Promise<void> {
    const grant = this.state.account?.spend?.grant ?? 0
    if (grant <= 0 || this.state.warnedGrant === grant) return
    this.state = { ...this.state, warnedGrant: grant }
    await this.writeState()
    this.broadcast()
  }

  // ---- approvals and holds on the stage (C1, C2) -----------------------------------

  /** The stage's answer to a question the agent asked; false when it was answered already. */
  decideApproval(id: string, approved: boolean, scope: ApprovalScope): boolean {
    const row = this.approvalDesk.list().find((a) => a.id === id)
    const ok = this.approvalDesk.decide(id, approved, scope)
    // "Always" on a hands step: the app it asked in is allowed from now on, in every conversation
    // ("always"), in this one only ("conversation") or in the runs of the task that asked
    // ("schedule"), until revoked on the Permissions page.
    if (ok && approved && scope !== 'once' && row?.app && (scope !== 'schedule' || row.scheduleId)) {
      const sessionId = scope === 'always' ? undefined : row.sessionId
      void this.grant(`computer_app:${row.app}`, sessionId, scope === 'schedule' ? row.scheduleId : undefined).catch((error: unknown) => this.ctx.logger.warn('nanomuse cloud: grant not kept: %s', message(error)))
    }
    return ok
  }

  /** The app a hands step of ours would act in: the one in front; '' for any other tool. */
  private handsApp(toolName: string): string {
    if (!isHandsTool(toolName) || !toolName.startsWith('mcp__nanomuse__')) return ''
    // nanoMuse's own window in front (the person was looking at the chat): no app to allow
    const app = appOf(this.stage.title)
    return isOwnApp(app) ? '' : app
  }

  /** Whether a standing grant covers a request right now: a hands step while the granted app is in front, in a conversation the grant holds in. */
  private granted(toolName: string, sessionId: string): boolean {
    return grantCovers(this.state.grants ?? [], this.handsApp(toolName), sessionId, this.scheduledTurns.get(sessionId) ?? [])
  }

  /** The one scheduled task whose run a hands step asks in, for *Always in this task*; '' outside such a run. */
  private scheduleAsking(toolName: string, sessionId: string): string {
    const ids = this.scheduledTurns.get(sessionId) ?? []
    return this.handsApp(toolName) && ids.length === 1 ? ids[0]! : ''
  }

  private async grant(target: string, sessionId?: string, scheduleId?: string): Promise<void> {
    const ctx = this.sessionCtx
    const sessionTitle = sessionId && ctx ? await this.sessionTitle(ctx, sessionId as SessionId, Date.now()) : ''
    const scheduleTitle = sessionId && scheduleId ? await this.scheduleTitle(sessionId, scheduleId) : ''
    const grant: Grant = { id: `g-${randomBytes(4).toString('hex')}`, target, at: Date.now(), ...(sessionId ? { sessionId, sessionTitle } : {}), ...(scheduleId ? { scheduleId, scheduleTitle } : {}) }
    this.state = { ...this.state, grants: withGrant(this.state.grants ?? [], grant) }
    await this.writeState()
    this.broadcast()
    if (sessionId) void this.refreshGrantTasks()
  }

  /**
   * What the lists say about the tasks behind the grants: which task grants' tasks the schedule
   * service no longer has, and which tasks live in the chat of a chat grant (it covers them too).
   */
  private async refreshGrantTasks(): Promise<void> {
    const schedule = this.ctx.get('schedule') as { list(request: { sessionId: SessionId }): Promise<{ id: string; title: string }[]> } | undefined
    if (!schedule) return
    const grants = (this.state.grants ?? []).filter((g) => g.sessionId)
    const gone: string[] = []
    const chatTasks: Record<string, string[]> = {}
    for (const sessionId of new Set(grants.map((g) => g.sessionId!))) {
      const list = await schedule.list({ sessionId: sessionId as SessionId }).catch(() => undefined)
      if (!list) continue
      const have = new Set(list.map((r) => r.id))
      for (const g of grants) if (g.sessionId === sessionId && g.scheduleId && !have.has(g.scheduleId)) gone.push(g.scheduleId)
      if (list.length && grants.some((g) => g.sessionId === sessionId && !g.scheduleId)) chatTasks[sessionId] = list.map((r) => r.title.slice(0, 60))
    }
    if (gone.join('|') === this.goneSchedules.join('|') && JSON.stringify(chatTasks) === JSON.stringify(this.chatTasks)) return
    this.goneSchedules = gone
    this.chatTasks = chatTasks
    this.broadcast()
  }

  /** A scheduled task's title, for the grant lists; '' when the schedule service does not know it. */
  private async scheduleTitle(sessionId: string, scheduleId: string): Promise<string> {
    const schedule = this.ctx.get('schedule') as { list(request: { sessionId: SessionId }): Promise<{ id: string; title: string }[]> } | undefined
    const list = await schedule?.list({ sessionId: sessionId as SessionId }).catch(() => [])
    return list?.find((r) => r.id === scheduleId)?.title.slice(0, 80) ?? ''
  }

  async revokeGrant(id: string): Promise<boolean> {
    const before = this.state.grants?.length ?? 0
    const grants = (this.state.grants ?? []).filter((g) => g.id !== id)
    if (grants.length === before) return false
    this.state = { ...this.state, grants }
    await this.writeState()
    this.broadcast()
    return true
  }

  /** Every remembered permission on this computer as one list (the Permissions page's *Standing grants*). */
  standingGrants(): StandingGrant[] {
    return standingGrants({ grants: this.state.grants ?? [], trusted: this.trusted, remoteControl: this.remoteControl, goneSchedules: this.goneSchedules, chatTasks: this.chatTasks })
  }

  /**
   * Revoke one standing grant by the id `standingGrants()` gave it, whichever store it lives
   * in: the hands' grant, a trusted device (`device:<id>`), or the remote-control switch.
   * False when nothing by that id is remembered.
   */
  async revokeStanding(id: string): Promise<boolean> {
    if (id === REMOTE_CONTROL_GRANT_ID) {
      if (!this.remoteControl) return false
      await this.setRemoteControl(false)
      return true
    }
    if (id.startsWith(DEVICE_GRANT_PREFIX)) {
      const deviceId = id.slice(DEVICE_GRANT_PREFIX.length)
      if (!this.state.trusted?.[deviceId]) return false
      await this.setTrusted(deviceId, '', false)
      return true
    }
    return this.revokeGrant(id)
  }

  /** The person takes the screen (`by: user`); the hands wait until `holdDone`. */
  takeHold(thread: string, tool: HoldTool, reason: string): Hold {
    return this.holdDesk.begin(thread, tool, 'user', reason)
  }

  holdDone(id: string): boolean {
    return this.holdDesk.done(id)
  }

  // ---- the hands model (C4) ----------------------------------------------------------

  /** The model the hands see the screen with: the person's choice, else the account's `gui` default, else the first sighted own model. */
  handsModel(): string {
    return this.handsChoice().model
  }

  /**
   * The hands model and where it lives (C11, order of 0.1.41): the person's choice when it still
   * exists — an own row's sighted model, or the account's `gui` model; else the chat provider's
   * `defaults.hands` when new chats answer through an own row that sees pictures; else the
   * account's `gui` model while signed in; else the first own row with a sighted model. The
   * runtime only speaks OpenAI's shape (and the ChatGPT backend), so an Anthropic or
   * native-Gemini row never drives the hands.
   */
  handsChoice(opts: { auto?: boolean } = {}): { provider: string; model: string } {
    this.reconcileOwnRows()
    // `auto`: the order as if nothing were chosen (what the *Automatic* entry would give)
    const chosen = opts.auto ? undefined : this.state.handsModel
    const own = this.state.providers ?? {}
    if (chosen && this.state.handsProvider) {
      const row = own[this.state.handsProvider]
      if (row && this.handsCapable(row) && row.models.some((m) => m.id === chosen && m.vision)) return { provider: this.state.handsProvider, model: chosen }
    }
    const models = this.cloudModels() ? (this.state.models ?? []) : []
    if (chosen && !this.state.handsProvider && models.some((m) => m.id === chosen && modelFor(m).includes('gui'))) return { provider: PROVIDER_ID, model: chosen }
    const chat = this.chatOwnRow()
    if (chat && this.handsCapable(chat.row)) {
      const model = this.ownModelFor(chat.row, 'hands')
      if (model) return { provider: chat.id, model }
    }
    const cloud = pickHandsModel(models)
    if (cloud) return { provider: PROVIDER_ID, model: cloud.id }
    for (const [id, row] of Object.entries(own)) {
      if (!this.handsCapable(row)) continue
      const sighted = row.models.find((m) => m.vision)
      if (sighted) return { provider: id, model: sighted.id }
    }
    return { provider: '', model: '' }
  }

  private handsCapable(row: OwnProvider): boolean {
    return row.capabilities.includes('vision') && (row.provider === CHATGPT_PROVIDER || apiOf(row.protocol, row.baseURL) === 'openai-completions')
  }

  /** The own row new chats answer through, when they do (the chat slot's provider is one of ours). */
  private chatOwnRow(): { id: string; row: OwnProvider } | undefined {
    const chat = this.chatChoice()
    const row = chat.provider ? this.state.providers?.[chat.provider] : undefined
    return row ? { id: chat.provider, row } : undefined
  }

  /**
   * The model an own row would get for a slot: the catalogue's `defaults.<slot>` when the row
   * lists it (or listed nothing, so the defaults stand in), else the row's first model with
   * that capability; '' when the row has none. `hands` wants a sighted model; `image` and
   * `video` their kinds.
   */
  private ownModelFor(row: OwnProvider, slot: Slot): string {
    const entry = this.catalogue.find((p) => p.id === row.provider)
    const fits = (m: OwnModel): boolean => (slot === 'chat' ? m.kind === 'chat' : slot === 'hands' ? m.vision : m.kind === slot)
    const capability: Capability = slot === 'hands' ? 'vision' : slot
    if (!row.capabilities.includes(capability)) return ''
    if (slot === 'hands' && !this.handsCapable(row)) return ''
    const wanted = entry?.defaults[slot]
    if (wanted && row.models.some((m) => m.id === wanted && fits(m))) return wanted
    const first = row.models.find(fits)
    if (first) return first.id
    // a video row lists no video models by name (Model Studio's `/models` leaves Wan out): the catalogue's default, else the known one
    if (slot === 'video') return wanted ?? DEFAULT_VIDEO_MODEL
    return ''
  }

  /** The sighted models the hands may use, the account's first (the recommended one marked), then each own row's (C11). */
  handsOptions(): ModelOption[] {
    this.reconcileOwnRows()
    const out: ModelOption[] = []
    if (this.cloudModels()) {
      const models = this.state.models ?? []
      const pick = pickHandsModel(models)
      for (const m of models) if (modelFor(m).includes('gui')) out.push({ provider: PROVIDER_ID, providerLabel: 'nanoMuse Cloud', id: m.id, name: m.name || m.id, ...(pick && m.id === pick.id ? { recommended: true } : {}) })
      out.sort((a, b) => Number(Boolean(b.recommended)) - Number(Boolean(a.recommended)))
    }
    for (const [id, row] of Object.entries(this.state.providers ?? {})) {
      if (!this.handsCapable(row)) continue
      for (const m of row.models) if (m.vision) out.push({ provider: id, providerLabel: row.label, id: m.id, name: m.name })
    }
    return out
  }

  /** The own rows that see pictures but cannot drive the hands (Anthropic, Google's native API): their labels, for the one sentence under the row. */
  handsExcluded(): string[] {
    return Object.values(this.state.providers ?? {}).filter((row) => row.capabilities.includes('vision') && !this.handsCapable(row)).map((row) => row.label)
  }

  /** The chat models new chats may answer through: the account's (the recommended one first and marked), then each own row's (C11). */
  chatOptions(): ModelOption[] {
    this.reconcileOwnRows()
    const out: ModelOption[] = []
    if (this.cloudModels()) {
      const models = this.state.models ?? []
      const pick = pickChatModel(models)
      for (const m of models) if (modelFor(m).includes('chat')) out.push({ provider: PROVIDER_ID, providerLabel: 'nanoMuse Cloud', id: m.id, name: m.name || m.id, ...(pick && m.id === pick.id ? { recommended: true } : {}) })
      out.sort((a, b) => Number(Boolean(b.recommended)) - Number(Boolean(a.recommended)))
    }
    for (const [id, row] of Object.entries(this.state.providers ?? {})) {
      if (!row.capabilities.includes('chat')) continue
      for (const m of row.models) if (m.kind === 'chat') out.push({ provider: id, providerLabel: row.label, id: m.id, name: m.name })
    }
    return out
  }

  /** Choose the hands model (`provider` an own row's id, or the account); the hands' MCP client is remounted with it (`hands-tools.ts`). */
  async setHandsModel(id: string, provider = PROVIDER_ID): Promise<void> {
    if (id && !this.handsOptions().some((o) => o.id === id && o.provider === provider)) throw new RelayError(400, 'bad_model', 'Not a hands model of the account or of an own key')
    this.state = { ...this.state, ...(id ? { handsModel: id } : {}), ...(id && provider !== PROVIDER_ID ? { handsProvider: provider } : {}) }
    if (!id) delete this.state.handsModel
    if (!id || provider === PROVIDER_ID) delete this.state.handsProvider
    await this.writeState()
    await this.writeHands()
    this.broadcast()
  }

  /**
   * What the bundled runtime's `[gui]` gets: the relay's OpenAI-style base, the hands model and
   * the account key — or, with an own key chosen (C11), that row's base URL and key (a local
   * server may have no key); with the ChatGPT sign-in, `provider: chatgpt`, which the runtime
   * answers from its own token store. Nothing when no sighted model is configured.
   */
  private async handsBody(): Promise<Record<string, string> | undefined> {
    const { provider, model } = this.handsChoice()
    if (provider === PROVIDER_ID) {
      const token = await this.token()
      if (token && this.state.account && model) return { provider: 'openai', model, base_url: this.relay.openaiBase, api_key: token }
      return undefined
    }
    const row = provider ? this.state.providers?.[provider] : undefined
    if (!row || !model) return undefined
    if (row.provider === CHATGPT_PROVIDER) return { provider: 'chatgpt', model }
    const apiKey = row.keyRef ? await this.credential(row.keyRef) : ''
    if (!apiKey && row.keyRef) return undefined
    return { provider: 'openai', model, base_url: row.baseURL, ...(apiKey ? { api_key: apiKey } : {}) }
  }

  /**
   * The runtime's `NANOMUSE_GUI_*` environment for `nanomuse mcp` (`hands-tools.ts` mounts the
   * MCP client with it, and mounts it again when this changes): the hands model, where it
   * lives and the key. `{}` when nothing is configured, so the runtime starts without hands.
   */
  async handsEnv(): Promise<Record<string, string>> {
    const body = await this.handsBody()
    if (!body?.model) return {}
    return { NANOMUSE_GUI_ENABLED: '1', NANOMUSE_GUI_PROVIDER: body.provider ?? 'openai', NANOMUSE_GUI_MODEL: body.model, NANOMUSE_GUI_BASE_URL: body.base_url ?? '', NANOMUSE_GUI_API_KEY: body.api_key ?? '' }
  }

  /** A hands call is in flight: the MCP client is not remounted under it. */
  handsBusy(): boolean {
    for (const call of this.calls.values()) if (call.name.startsWith('mcp__nanomuse__')) return true
    return false
  }

  // ---- the work a protected exit ends (src/work.ts) ------------------------------------

  /**
   * The work a real quit (later, a restart-update) would end, over every source there is:
   * the turns of all sessions, the hands' calls, the tasks other devices asked for and the
   * approvals waiting on this screen, one line per kind (src/work.ts's aggregateWork).
   */
  private workBusy(): WorkItem[] {
    return aggregateWork({
      turnSessions: this.turns.sessions(),
      taskRuns: this.tasks?.origins() ?? [],
      handsBusy: this.handsBusy(),
      approvals: this.approvalDesk.list().length,
    })
  }

  /**
   * The gate for new work while the exit hands over: closed, a task another device sends is
   * refused with the usual `busy` error and a turn that opens all the same is stopped at once.
   */
  setWorkGate(open: boolean): boolean {
    this.gateClosed = !open
    this.ctx.logger.info('nanomuse cloud: the exit gate is %s', this.gateClosed ? 'closed; new work is refused until it opens' : 'open')
    return !this.gateClosed
  }

  /** Stop the turn on one session, whatever it is, and wait for the agent to go quiet. */
  private async abortSession(sessionId: string, cause: AgentCancelCause): Promise<void> {
    const controller = this.sessionCtx?.sessionController
    if (!controller) return
    const resolved = await controller.resolveAgent(sessionId as SessionId).catch(() => undefined)
    if (!resolved || !('agent' in resolved)) return
    resolved.agent.cancel(cause)
    await resolved.agent.whenIdle().catch(() => undefined)
  }

  /**
   * End the work in progress (the person confirmed the exit): every turn is cancelled,
   * which settles the tasks' hub calls, the hands' holds and the approvals waiting, and
   * the aggregate is checked again until it is empty or the rounds run out. The shell's
   * exit waits for this, and only then stops the Host.
   */
  async stopAllWork(): Promise<WorkItem[]> {
    this.gateClosed = true
    for (let round = 0; round < 5; round++) {
      const busy = this.workBusy()
      if (busy.length === 0) return []
      if (round > 0) this.ctx.logger.warn('nanomuse cloud: work still on for the exit (round %d): %s', round, busy.map((item) => item.kind).join(', '))
      for (const sessionId of this.turns.sessions()) await this.abortSession(sessionId, { kind: 'user' })
    }
    return this.workBusy()
  }

  /**
   * `hands-tools.ts` says how its last mount of the hands' MCP client went: the Computer-use
   * page and the Connectors row show the reason when the hands are off. `at` 0 before the
   * first mount.
   */
  handsMounted(outcome: { ok: boolean; reason?: string }): void {
    this.handsMount = { at: Date.now(), ok: outcome.ok, reason: outcome.ok ? '' : (outcome.reason ?? 'the hands did not mount').slice(0, 300) }
    this.broadcast()
  }

  /** The last mount of the hands' MCP client: `reason` is empty when it went well, else why it did not. */
  handsStatus(): HandsMount {
    return { ...this.handsMount }
  }

  /**
   * `$DSH_HOME/nanomuse/hands.json` (0600, next to `cloud.json`): the same body as `handsEnv`,
   * kept for anything that reads the file (the hands check, a person looking); the preset of
   * 0.1.40 read it once at start, `hands-tools.ts` now takes the environment straight from the
   * service. Removed when nothing is configured.
   */
  private async writeHands(): Promise<void> {
    const path = join(this.dir(), 'hands.json')
    const body = await this.handsBody()
    if (!body) {
      await rm(path, { force: true }).catch(() => undefined)
      return
    }
    await mkdir(this.dir(), { recursive: true })
    await writeFile(path, JSON.stringify(body, null, 2) + '\n', { mode: 0o600 })
  }

  private async credential(ref: string): Promise<string> {
    try {
      return (await this.ctx.credentials.resolve(credentialRef(ref)))?.value ?? ''
    } catch {
      return ''
    }
  }

  // ---- own keys and the ChatGPT sign-in (C11) ------------------------------------------

  /** The region the "ways on" are ordered for: the relay's word for the account when signed in, else the UI language's hint. */
  private region(lang: string): Region {
    return regionOf(this.state.account?.region, lang)
  }

  /** What the account can do, as the relay's model list says. Public Cloud media is retired. */
  private cloudCapabilities(): Capability[] {
    if (!this.cloudModels()) return []
    const models = this.state.models ?? []
    const out: Capability[] = []
    if (models.some((m) => modelFor(m).includes('chat'))) out.push('chat')
    if (models.some((m) => modelFor(m).includes('gui'))) out.push('vision')
    if (relayMediaEnabled(this.relay.origin)) {
      if (models.some((m) => m.kind === 'image')) out.push('image')
      if (models.some((m) => m.kind === 'video')) out.push('video')
    }
    return out
  }

  /** What everything configured can do between them (C11, the rule). */
  capabilities(): Capability[] {
    const sources = [{ id: PROVIDER_ID, label: 'nanoMuse Cloud', capabilities: this.cloudCapabilities() }, ...Object.entries(this.state.providers ?? {}).map(([id, row]) => ({ id, label: row.label, capabilities: row.capabilities }))]
    const set = capabilitiesOf(sources)
    return CAPABILITIES.filter((c) => set.has(c))
  }

  /** The own rows that have a capability. */
  private ownWith(capability: Capability): Array<[string, OwnProvider]> {
    return Object.entries(this.state.providers ?? {}).filter(([, row]) => row.capabilities.includes(capability))
  }

  /** `GET /providers`: the catalogue, the rows, the capabilities, the sign-in — for the "ways on" and the pickers. */
  async providersView(lang: string): Promise<ProvidersView> {
    this.reconcileOwnRows()
    const runtime = await runtimeInfo().catch(() => undefined)
    return {
      region: this.region(lang),
      catalogue: this.catalogue,
      configured: Object.values(this.state.providers ?? {}),
      capabilities: this.capabilities(),
      cloud: { signedIn: this.signedInCache && Boolean(this.state.account), capabilities: this.cloudCapabilities() },
      chatgpt: this.chatGptView(runtime?.ok === true),
      hands: this.handsChoice(),
      chat: this.chatChoice(),
    }
  }

  private chatGptView(runtime: boolean): ProvidersView['chatgpt'] {
    return { signedIn: Boolean(this.state.chatgpt), label: this.state.chatgpt?.label ?? '', proxy: Boolean(this.chatgpt.ready), login: this.chatgpt.login, runtime }
  }

  /**
   * `POST /providers/save`: a key for a catalogue entry (or a hand-made endpoint). The key goes to
   * the credential store as `NANOMUSE_KEY_<ID>`; the row — base URL, the credential's name, the
   * chat models the endpoint listed (or the catalogue's defaults) — goes into the harness's model
   * adapter the way the Models page writes one. The row's capabilities are the catalogue's;
   * `custom` takes what the person says.
   *
   * A vendor has one row, named after its catalogue id, so saving its key again replaces it.
   * A hand-made endpoint (`custom`) is one row per endpoint (#294): without `row`, a new one
   * joins under the lowest free id (`custom`, `custom-2`, ...); with `row`, that row is the one
   * changed, and an empty key keeps the key it has.
   */
  async saveProvider(input: { id: string; row?: string; apiKey?: string; baseURL?: string; label?: string; capabilities?: string[]; models?: string[]; catalogOnly?: boolean; lang?: string }): Promise<OwnProvider> {
    this.reconcileOwnRows()
    const id = input.id.trim().toLowerCase()
    if (id === CHATGPT_PROVIDER || id === PROVIDER_ID) throw new RelayError(400, 'bad_provider', 'That row is written by a sign-in, not a key')
    const entry = this.catalogue.find((p) => p.id === id)
    if (!entry) throw new RelayError(404, 'unknown_provider', 'Not in the catalogue')
    const wantedRow = (input.row ?? '').trim().toLowerCase()
    const existing = wantedRow ? this.state.providers?.[wantedRow] : this.state.providers?.[id]
    if (wantedRow && (!existing || existing.provider !== id)) throw new RelayError(404, 'not_found', 'No such row')
    const rowId = wantedRow || (entry.id === 'custom' ? this.freeCustomRowId() : id)
    const region = this.region(input.lang ?? '')
    const baseURL = (input.baseURL ?? '').trim().replace(/\/+$/, '') || (wantedRow ? existing?.baseURL : undefined) || baseUrlFor(entry, region)
    if (!/^https?:\/\//.test(baseURL)) throw new RelayError(400, 'bad_url', 'The address must start with http:// or https://')
    // `user:pass@` would be kept in cloud.json and the model adapter's row and shown back as typed
    if (hasUserinfo(baseURL)) throw new RelayError(400, 'url_userinfo', USERINFO_TEXT)
    let apiKey = (input.apiKey ?? '').trim()
    // a row changed without a new key keeps the one it has, read back to ask the endpoint for its models
    const keptKeyRef = !apiKey && wantedRow && existing?.keyRef ? existing.keyRef : ''
    if (keptKeyRef) apiKey = await this.credential(keptKeyRef)
    if (!apiKey && !entry.auth.includes('none')) throw new RelayError(400, 'no_key', 'This provider needs a key')
    // not `bad_key`: the client reads that code as a retired sign-in
    if (apiKey && /\s/.test(apiKey)) throw new RelayError(400, 'key_shape', 'A key has no spaces in it')
    let capabilities: Capability[] = entry.capabilities
    if (entry.user_capabilities) {
      const said = (input.capabilities ?? (wantedRow ? existing?.capabilities : undefined) ?? []).filter((c): c is Capability => (CAPABILITIES as readonly string[]).includes(c))
      capabilities = CAPABILITIES.filter((c) => c === 'chat' || said.includes(c))
    }
    // A vendor that wants a session id per conversation (OpenCode Go) is reached through the
    // model adapter's catalog route of the same id, which sends it: the models are the ones that
    // route serves, and a typed id it does not know is left out (the adapter would refuse the row).
    const catalogRoute = Boolean(entry.session_header)
    const served = catalogRoute ? await this.catalogRouteModels(id) : []
    const listed = catalogRoute ? served.map((m) => ({ id: m })) : await listModelRows(entry.protocol, baseURL, apiKey)
    // The ids the person typed (a gateway that does not answer `GET /models`, or one model
    // wanted out of hundreds): first in the list, the endpoint's own after them; a typed id the
    // list also has keeps what the list said about it (OpenRouter's thinking levels). A row
    // changed without new ids keeps the ones it had; when the person trimmed the row's model
    // catalog (or asked for `catalogOnly`), only the kept chat models stay (#525).
    const keepCatalogOnly = input.catalogOnly === true || (input.models === undefined && existing?.customModels === true)
    const existingChatIds = existing?.models.filter((m) => m.kind === 'chat').map((m) => m.id)
    const typedInput = input.models ?? (wantedRow || existing?.customModels ? existingChatIds : undefined)
    const typed = typedModelIds(typedInput).filter((m) => !catalogRoute || served.includes(m))
    const typedRows = typed.map((mId) => listed.find((m) => m.id === mId) ?? existing?.models.find((m) => m.id === mId) ?? { id: mId })
    const restRows = keepCatalogOnly && typed.length
      ? listed.filter((m) => !typed.includes(m.id) && kindOf(m.id) !== 'chat' && m.id !== entry.defaults.chat)
      : listed.filter((m) => !typed.includes(m.id))
    let models = modelsOf({ ...entry, capabilities }, [...typedRows, ...restRows])
    if (entry.user_capabilities) models = models.map((m) => ({ ...m, vision: capabilities.includes('vision') }))
    // A row with no chat model cannot be served (the model adapter refuses a route that resolves
    // no models) and nothing could pick from it: a plain sentence, before anything is written.
    if (!models.some((m) => m.kind === 'chat')) throw new RelayError(400, 'no_models', 'The endpoint did not list its models (GET /models). Type the ids of the models it serves, separated by commas.')
    const keyRef = keptKeyRef || (apiKey ? keyRefFor(rowId) : '')
    if (keyRef && !keptKeyRef) await this.ctx.credentials.set(credentialRef(keyRef), apiKey)
    // a hand-made endpoint reads as its host unless named, so several are told apart
    const fallbackLabel = entry.id === 'custom' ? hostNameOf(baseURL) : input.lang?.toLowerCase().startsWith('zh') ? entry.name_zh : entry.name
    const label = (input.label ?? '').trim() || (wantedRow ? existing?.label : undefined) || fallbackLabel
    const row: OwnProvider = {
      id: rowId,
      provider: id,
      label,
      protocol: entry.protocol,
      baseURL,
      keyRef,
      capabilities,
      models,
      at: Date.now(),
      ...(catalogRoute ? { catalogRoute } : {}),
      ...(keepCatalogOnly && typed.length ? { customModels: true } : {}),
    }
    await this.ctx.settings.update(LLM_ROW, { providers: { [rowId]: ownProviderRow(row, entry.reasoning) } })
    this.state = { ...this.state, providers: { ...this.state.providers, [rowId]: row } }
    await this.writeState()
    await this.adoptOwnDefault(rowId, row)
    await this.alignTrimmedChatSelection(this.state.providers ?? {})
    await this.writeHands()
    this.broadcast()
    return row
  }

  /** The lowest free id for a hand-made endpoint's row: `custom`, then `custom-2`, `custom-3`, ... (#294). */
  private freeCustomRowId(): string {
    const taken = this.state.providers ?? {}
    if (!taken.custom) return 'custom'
    for (let n = 2; ; n++) if (!taken[`custom-${n}`]) return `custom-${n}`
  }

  /**
   * `POST /providers/rename`: a new name for an own row, in the pickers and the model adapter
   * alike; empty goes back to the row's host (a hand-made endpoint) or the catalogue's name.
   */
  async renameProvider(id: string, label: string, lang = ''): Promise<OwnProvider> {
    this.reconcileOwnRows()
    const row = this.state.providers?.[id]
    if (!row || id === CHATGPT_PROVIDER) throw new RelayError(404, 'not_found', 'No such row')
    const entry = this.catalogue.find((p) => p.id === row.provider)
    const name = label.trim().slice(0, 80) || (row.provider === 'custom' ? hostNameOf(row.baseURL) : lang.toLowerCase().startsWith('zh') ? entry?.name_zh : entry?.name) || id
    const renamed: OwnProvider = { ...row, label: name }
    await this.ctx.settings.update(LLM_ROW, { providers: { [id]: ownProviderRow(renamed, entry?.reasoning) } })
    this.state = { ...this.state, providers: { ...this.state.providers, [id]: renamed } }
    await this.writeState()
    this.broadcast()
    return renamed
  }

  /**
   * Keep each own-key row's chat models in `state.providers` aligned with what the model
   * adapter's settings row (`llm-pi-ai` `providers.<id>.models`) currently lists, when the
   * person trimmed or edited the catalog there (#525). Non-chat (`image`, `video`) models
   * stay on the row; a model's learned `reasoning` levels and `vision` capability are kept.
   */
  private reconcileOwnRows(): boolean {
    const providers = this.state.providers
    if (!providers) return false
    let written: Record<string, unknown> | undefined
    try {
      const current = this.ctx.settings.describe().find((d) => d.ns === LLM_ROW)
      written = (current?.value as { providers?: Record<string, unknown> } | undefined)?.providers
    } catch {
      written = undefined
    }
    if (!written) return false
    let nextProviders: Record<string, OwnProvider> | undefined
    for (const [id, row] of Object.entries(providers)) {
      if (id === CHATGPT_PROVIDER || id === PROVIDER_ID) continue
      const raw = written[id]
      if (!raw || typeof raw !== 'object') continue
      const rawModels = (raw as { models?: unknown }).models
      if (!Array.isArray(rawModels)) continue
      const entry = this.catalogue.find((p) => p.id === row.provider)
      const chatNow = row.models.filter((m) => m.kind === 'chat')
      const seen = new Set<string>()
      const nextChat: OwnModel[] = []
      for (const item of rawModels) {
        const m = typeof item === 'string' ? { id: item } : item && typeof item === 'object' ? (item as { id?: unknown; displayName?: unknown; input?: unknown }) : undefined
        const modelId = typeof m?.id === 'string' ? m.id.trim() : ''
        if (!modelId || seen.has(modelId)) continue
        seen.add(modelId)
        const prev = chatNow.find((x) => x.id === modelId)
        const displayName = typeof m?.displayName === 'string' ? m.displayName.trim() : ''
        const input = Array.isArray(m?.input) ? m.input : undefined
        const vision = row.capabilities.includes('vision') && (
          (input ? input.includes('image') : false) ||
          (prev ? prev.vision : entry?.user_capabilities ? true : entry ? sighted({ ...entry, capabilities: row.capabilities }, modelId) : false)
        )
        nextChat.push({
          id: modelId,
          name: displayName || prev?.name || modelId,
          vision,
          kind: 'chat',
          ...(prev?.reasoning ? { reasoning: prev.reasoning } : {}),
        })
      }
      if (!nextChat.length) continue
      const sameChat = nextChat.length === chatNow.length && nextChat.every((m, i) => m.id === chatNow[i]?.id && m.name === chatNow[i]?.name && m.vision === chatNow[i]?.vision)
      if (sameChat) continue
      nextProviders ??= { ...providers }
      nextProviders[id] = {
        ...row,
        models: [...nextChat, ...row.models.filter((m) => m.kind !== 'chat')],
        customModels: true,
      }
    }
    if (!nextProviders) return false
    this.state = { ...this.state, providers: nextProviders }
    void this.alignTrimmedChatSelection(nextProviders)
    void this.writeState().catch((error: unknown) => this.ctx.logger.warn('nanomuse: state write failed: %s', message(error)))
    void this.writeHands().catch((error: unknown) => this.ctx.logger.warn('nanomuse: hands model not written: %s', message(error)))
    return true
  }

  /** Move the chat slot onto a remaining model of the row when its previous model was trimmed out of the catalog. */
  private async alignTrimmedChatSelection(providers: Record<string, OwnProvider>): Promise<void> {
    const svc = this.defaultModelService()
    if (!svc) return
    try {
      const current = svc.currentSelection()
      const row = providers[current.provider]
      if (!row || row.models.some((m) => m.kind === 'chat' && m.id === current.model)) return
      const nextModel = this.ownModelFor(row, 'chat')
      if (!nextModel) return
      await svc.saveSelection({ provider: current.provider, model: nextModel })
      await this.mainChatFollows(current.provider, nextModel)
    } catch {
      // best-effort alignment
    }
  }

  /**
   * Remove pre-migration own-provider routes and their exact credential refs once on this
   * computer. Ownership comes from the persisted plugin row plus the provider catalogue; the
   * adapter settings are read before mutation so only persisted rows are unset. No credential
   * store enumeration or prefix matching is used.
   */
  private async cleanupOwnProviderKeys(): Promise<void> {
    const saved = this.state.ownProviderKeyCleanup as { version?: unknown; status?: unknown; targets?: unknown } | undefined
    if (saved) {
      if (saved.version !== 1 || (saved.status !== 'pending' && saved.status !== 'complete')) {
        throw new RelayError(503, 'own_provider_cleanup_failed', 'Own-provider cleanup state is invalid. Restart to retry cleanup.')
      }
      if (saved.status === 'complete') return
    }

    if (!this.catalogue.length) {
      if (!saved) {
        this.state = { ...this.state, ownProviderKeyCleanup: { version: 1, status: 'pending', targets: [] } }
        await this.persistOwnProviderCleanupState()
      }
      return
    }

    let targets = saved?.status === 'pending'
      ? this.ownProviderCleanupTargetsFrom(saved.targets)
      : this.ownProviderCleanupTargetsFromState()
    if (!saved) {
      this.state = { ...this.state, ownProviderKeyCleanup: { version: 1, status: 'pending', targets } }
      await this.persistOwnProviderCleanupState()
    }
    let described: { value?: unknown } | undefined
    try {
      described = this.ctx.settings.describe().find((d) => d.ns === LLM_ROW)
    } catch {
      if (targets.length) await this.disableOwnProviderReferences(targets)
      return
    }
    if (!described) {
      if (targets.length) await this.disableOwnProviderReferences(targets)
      // Do not mark complete without seeing persisted adapter rows; the next start can retry.
      return
    }
    const adapterProviders = (described?.value as { providers?: Record<string, unknown> } | undefined)?.providers ?? {}
    const fromAdapter = this.ownProviderCleanupTargetsFromAdapter(adapterProviders)
    const byId = new Map(targets.map((target) => [target.id, target]))
    for (const target of fromAdapter) if (!byId.has(target.id)) byId.set(target.id, target)
    targets = [...byId.values()]
    const pending = { version: 1 as const, status: 'pending' as const, targets }
    this.state = { ...this.state, ownProviderKeyCleanup: pending }
    await this.persistOwnProviderCleanupState()
    if (!targets.length) {
      this.state = { ...this.state, ownProviderKeyCleanup: { version: 1, status: 'complete' } }
      await this.persistOwnProviderCleanupState()
      return
    }

    const removeRows = targets.filter((target) => Object.hasOwn(adapterProviders, target.id))
    if (removeRows.length) {
      try {
        await this.ctx.settings.mutate(LLM_ROW, removeRows.map(({ id }) => ({ op: 'unset' as const, path: ['providers', id] })))
      } catch {
        this.ctx.logger.warn('nanomuse: own-provider routes could not be disabled; cleanup will retry on the next start')
        throw new RelayError(503, 'own_provider_cleanup_failed', 'Own-provider routes could not be disabled. Restart to retry cleanup.')
      }
    }

    await this.disableOwnProviderReferences(targets)

    const failed: OwnProviderKeyCleanupTarget[] = []
    for (const target of targets) {
      try {
        await this.ctx.credentials.unset(credentialRef(target.keyRef))
      } catch {
        failed.push(target)
        this.ctx.logger.warn('nanomuse: own-provider credential cleanup failed for row %s; retry will run on the next start', target.id)
      }
    }
    if (failed.length) {
      this.state = { ...this.state, ownProviderKeyCleanup: { version: 1, status: 'pending', targets: failed } }
      await this.persistOwnProviderCleanupState()
      throw new RelayError(503, 'own_provider_cleanup_failed', 'Some own-provider credentials could not be deleted. Restart to retry cleanup.')
    }

    const completed = { ...this.state, ownProviderKeyCleanup: { version: 1 as const, status: 'complete' as const } }
    this.state = completed
    try {
      await this.persistOwnProviderCleanupState()
    } catch {
      this.state = { ...completed, ownProviderKeyCleanup: pending }
      throw new RelayError(503, 'own_provider_cleanup_failed', 'Cleanup completion could not be saved. Restart to retry cleanup.')
    }
    this.ctx.logger.info('nanomuse: old own-provider routes and credentials were cleaned up')
  }

  /** Keep in-memory and runtime-owned references off target rows before any credential deletion. */
  private async disableOwnProviderReferences(targets: OwnProviderKeyCleanupTarget[]): Promise<void> {
    const providers = { ...this.state.providers }
    for (const { id } of targets) delete providers[id]
    this.state = { ...this.state, providers }
    if (this.state.handsProvider && targets.some(({ id }) => id === this.state.handsProvider)) {
      delete this.state.handsProvider
      delete this.state.handsModel
    }
    if (this.state.media) {
      const media: MediaState = { ...this.state.media }
      if (media.imageProvider && targets.some(({ id }) => id === media.imageProvider)) {
        delete media.imageProvider
        delete media.imageModel
      }
      if (media.videoProvider && targets.some(({ id }) => id === media.videoProvider)) {
        delete media.videoProvider
        delete media.videoModel
      }
      this.state = { ...this.state, media }
    }

    // The persisted adapter rows are now gone (or not ready yet); keep other routes and move
    // the global chat slot and hands config before any credential deletion.
    for (const { id } of targets) await this.releaseChatRoute(id, true)
    const modelService = this.defaultModelService()
    if (modelService) {
      let selection: { provider: string; model: string }
      try {
        selection = modelService.currentSelection()
      } catch {
        throw new RelayError(503, 'own_provider_cleanup_failed', 'The chat route could not be checked. Restart to retry cleanup.')
      }
      if (targets.some(({ id }) => id === selection.provider)) {
        throw new RelayError(503, 'own_provider_cleanup_failed', 'The chat route could not be disabled. Restart to retry cleanup.')
      }
    }
    try {
      await this.writeHands()
      await this.persistOwnProviderCleanupState()
    } catch {
      throw new RelayError(503, 'own_provider_cleanup_failed', 'Own-provider references could not be saved. Restart to retry cleanup.')
    }
  }

  /** Only a catalogue-owned key provider, excluding Cloud and ChatGPT, can enter this ledger. */
  private ownProviderCleanupTargetsFromState(): OwnProviderKeyCleanupTarget[] {
    const out: OwnProviderKeyCleanupTarget[] = []
    for (const [id, row] of Object.entries(this.state.providers ?? {})) {
      if (id === CHATGPT_PROVIDER || id === PROVIDER_ID || row.provider === CHATGPT_PROVIDER || row.provider === PROVIDER_ID) continue
      const keyRef = typeof row.keyRef === 'string' ? row.keyRef.trim() : ''
      const entry = this.catalogue.find((provider) => provider.id === row.provider)
      if (!keyRef || !entry?.auth.includes('key') || keyRef !== keyRefFor(id)) continue
      out.push({ id, provider: row.provider, keyRef })
    }
    return out
  }

  /** Adapter-only rows still identify ownership through the catalogue id and our exact generated ref. */
  private ownProviderCleanupTargetsFromAdapter(raw: Record<string, unknown>): OwnProviderKeyCleanupTarget[] {
    const out: OwnProviderKeyCleanupTarget[] = []
    for (const [id, value] of Object.entries(raw)) {
      if (id === CHATGPT_PROVIDER || id === PROVIDER_ID || !value || typeof value !== 'object') continue
      const keyRef = (value as { apiKeyEnv?: unknown }).apiKeyEnv
      if (typeof keyRef !== 'string' || !keyRef || keyRef !== keyRefFor(id)) continue
      const provider = this.catalogue.find((entry) => entry.id === id && entry.auth.includes('key'))
        ?? (/^custom(?:-[2-9]\d*)?$/.test(id) ? this.catalogue.find((entry) => entry.id === 'custom' && entry.auth.includes('key')) : undefined)
      if (!provider) continue
      const stateRow = this.state.providers?.[id]
      if (stateRow && stateRow.provider !== provider.id) continue
      out.push({ id, provider: provider.id, keyRef })
    }
    return out
  }

  /** Retry ledger data is validated again before any adapter or credential mutation. */
  private ownProviderCleanupTargetsFrom(raw: unknown): OwnProviderKeyCleanupTarget[] {
    if (!Array.isArray(raw)) throw new RelayError(503, 'own_provider_cleanup_failed', 'Own-provider cleanup retry data is invalid. Restart to retry cleanup.')
    const targets: OwnProviderKeyCleanupTarget[] = []
    for (const value of raw) {
      if (!value || typeof value !== 'object') throw new RelayError(503, 'own_provider_cleanup_failed', 'Own-provider cleanup retry data is invalid. Restart to retry cleanup.')
      const target = value as Partial<OwnProviderKeyCleanupTarget>
      const entry = typeof target.provider === 'string' ? this.catalogue.find((provider) => provider.id === target.provider) : undefined
      if (typeof target.id !== 'string' || !target.id || target.id === CHATGPT_PROVIDER || target.id === PROVIDER_ID || target.provider === CHATGPT_PROVIDER || target.provider === PROVIDER_ID || typeof target.keyRef !== 'string' || target.keyRef !== keyRefFor(target.id) || !entry?.auth.includes('key')) {
        throw new RelayError(503, 'own_provider_cleanup_failed', 'Own-provider cleanup retry data is invalid. Restart to retry cleanup.')
      }
      targets.push({ id: target.id, provider: target.provider!, keyRef: target.keyRef })
    }
    return targets
  }

  private async persistOwnProviderCleanupState(): Promise<void> {
    try {
      await this.writeState()
    } catch {
      throw new RelayError(503, 'own_provider_cleanup_failed', 'Own-provider cleanup state could not be saved. Restart to retry cleanup.')
    }
  }

  /**
   * The saved own-key rows written to the model adapter again, where the row it has differs from
   * what the catalogue now says (a vendor's thinking levels learned after the key was saved).
   * The adapter's settings entry may not be registered yet while the host starts: a few quiet
   * retries, then the next start.
   */
  private refreshOwnRows(attempt = 0): void {
    if (this.state.ownProviderKeyCleanup?.status === 'pending') {
      if (attempt < 4) {
        setTimeout(() => {
          void this.cleanupOwnProviderKeys()
            .then(() => this.refreshOwnRows(attempt + 1))
            .catch(() => {
              this.ctx.logger.warn('nanomuse: own-provider cleanup remains pending; it will retry on the next start')
              this.refreshOwnRows(attempt + 1)
            })
        }, 1000 * (attempt + 1)).unref?.()
      }
      return
    }
    this.reconcileOwnRows()
    const rows = Object.entries(this.state.providers ?? {}).filter(([id]) => id !== CHATGPT_PROVIDER && id !== PROVIDER_ID)
    if (!rows.length) return
    let current: { value?: unknown } | undefined
    try {
      current = this.ctx.settings.describe().find((d) => d.ns === LLM_ROW)
    } catch {
      current = undefined
    }
    if (!current) {
      if (attempt < 4) setTimeout(() => this.refreshOwnRows(attempt + 1), 3000 * (attempt + 1)).unref?.()
      return
    }
    const written = (current.value as { providers?: Record<string, unknown> } | undefined)?.providers ?? {}
    void (async () => {
      for (const [id, row] of rows) {
        const entry = this.catalogue.find((p) => p.id === row.provider)
        const wanted = ownProviderRow(row, entry?.reasoning)
        if (canonical(written[id] ?? null) === canonical(wanted)) continue
        await this.ctx.settings.update(LLM_ROW, { providers: { [id]: wanted } }).catch((error: unknown) => this.ctx.logger.warn('nanomuse: own-key row %s not refreshed: %s', id, message(error)))
      }
    })()
  }

  /**
   * The "Use it for" card (0.1.41): the slots an own row could take and the model each would
   * get — the catalogue's `defaults.<slot>` when the row lists it, else its first model with
   * the capability. A slot the row has no model for is left out, so the card shows no toggle for it.
   */
  slotOffer(id: string): SlotOffer {
    this.reconcileOwnRows()
    const row = this.state.providers?.[id]
    if (!row) return {}
    const offer: SlotOffer = {}
    for (const slot of SLOTS) {
      const model = this.ownModelFor(row, slot)
      if (model) offer[slot] = model
    }
    return offer
  }

  /**
   * *Use it*: every ticked slot switches to the row, model as `slotOffer` says, through the
   * same setters the Models page uses. Unticked slots change nothing. Returns what was set.
   */
  async adoptProvider(id: string, slots: Slot[]): Promise<SlotOffer> {
    const row = this.state.providers?.[id]
    if (!row) throw new RelayError(404, 'not_found', 'No such row')
    const offer = this.slotOffer(id)
    const done: SlotOffer = {}
    for (const slot of SLOTS) {
      const model = offer[slot]
      if (!slots.includes(slot) || !model) continue
      if (slot === 'chat') await this.setChatModel(model, id)
      else if (slot === 'hands') await this.setHandsModel(model, id)
      else if (slot === 'image') await this.setImageModel(model, id)
      else await this.setVideoModel(model, id)
      done[slot] = model
    }
    return done
  }

  /** New chats answer through the first own key when the default is still dsh's stock DeepSeek without a key (as `adoptDefaultModel` does for the account). */
  private async adoptOwnDefault(id: string, row: OwnProvider): Promise<void> {
    const pick = row.models.find((m) => m.kind === 'chat')
    const svc = this.defaultModelService()
    if (!pick || !svc) return
    try {
      const current = svc.currentSelection()
      if (current.provider !== 'deepseek-official' && current.provider !== 'deepseek-account') return
      if ((await this.ctx.credentials.resolve(credentialRef('DEEPSEEK_API_KEY')))?.value) return
      const entry = this.catalogue.find((p) => p.id === row.provider)
      const model = entry?.defaults.chat && row.models.some((m) => m.id === entry.defaults.chat) ? entry.defaults.chat : pick.id
      await svc.saveSelection({ provider: id, model })
      this.ctx.logger.info('nanomuse: new sessions answer through %s/%s', id, model)
      await this.mainChatFollows(id, model)
    } catch (error: unknown) {
      this.ctx.logger.warn('nanomuse: could not make the own key the default: %s', message(error))
    }
  }

  /**
   * `POST /providers/remove`: the row, the credential and the choices that pointed at it.
   * `cloudModelsOn` is true when the row was the last chat source while the account's models
   * were off, so they are on again (the person reads one sentence under the row).
   */
  async removeProvider(id: string): Promise<{ cloudModelsOn: boolean }> {
    if (id === CHATGPT_PROVIDER) {
      await this.chatGptLogout()
      return { cloudModelsOn: false }
    }
    const row = this.state.providers?.[id]
    if (!row) throw new RelayError(404, 'not_found', 'No such row')
    const cloudModelsOn = await this.dropOwnRow(id, row)
    this.broadcast()
    return { cloudModelsOn }
  }

  /** Removes the row and what pointed at it; true when the account's models had to come back on. */
  private async dropOwnRow(id: string, row: OwnProvider): Promise<boolean> {
    // Keep the exact model-adapter row so a credential-store failure can restore it without
    // silently changing hand-edited fields. The key itself lives only in the credential store.
    const described = this.ctx.settings.describe().find((d) => d.ns === LLM_ROW)
    const persistedProviders = (described?.value as { providers?: Record<string, unknown> } | undefined)?.providers
    const originalRow = persistedProviders?.[id]
    try {
      await this.ctx.settings.mutate(LLM_ROW, [{ op: 'unset', path: ['providers', id] }])
    } catch {
      this.ctx.logger.warn('nanomuse: provider row %s was not removed; its credential was left untouched', id)
      throw new RelayError(502, 'provider_remove_failed', 'The provider could not be removed from settings. No credential was deleted; retry the removal.')
    }
    if (row.keyRef) {
      try {
        await this.ctx.credentials.unset(credentialRef(row.keyRef))
      } catch {
        // The persisted provider row is gone, but removing its credential failed. Restore
        // the row and keep the in-memory/cloud.json state unchanged so the user can retry.
        // Never include the credential value or provider error text in logs or responses.
        try {
          await this.ctx.settings.update(LLM_ROW, {
            providers: { [id]: originalRow ?? ownProviderRow(row, this.catalogue.find((p) => p.id === row.provider)?.reasoning) },
          })
        } catch {
          this.ctx.logger.error('nanomuse: provider row %s could not be restored after credential deletion failed; retry removal', id)
        }
        this.ctx.logger.warn('nanomuse: credential for provider row %s was not deleted; removal can be retried', id)
        throw new RelayError(502, 'credential_delete_failed', 'The provider credential could not be deleted, so removal was not completed. Retry the removal.')
      }
    }
    const providers = { ...this.state.providers }
    delete providers[id]
    this.state = { ...this.state, providers }
    if (this.state.handsProvider === id) {
      delete this.state.handsProvider
      delete this.state.handsModel
    }
    if (this.state.media?.imageProvider === id || this.state.media?.videoProvider === id) {
      const media: MediaState = { ...this.state.media }
      if (media.imageProvider === id) {
        delete media.imageProvider
        delete media.imageModel
      }
      if (media.videoProvider === id) {
        delete media.videoProvider
        delete media.videoModel
      }
      this.state = { ...this.state, media }
    }
    const cloudModelsOn = await this.releaseChatRoute(id)
    await this.writeState()
    await this.writeHands()
    return cloudModelsOn
  }

  /**
   * A provider route is going away (an own row removed, the account signed out). A chat slot
   * or the main chat still naming it would send every turn to a route no adapter serves, and
   * the person would read `no adapter registered for provider "custom"` (NO_ADAPTER) with
   * nothing to do about it. The slot moves to the first chat model left (the account's, then
   * the own rows'). When nothing is left because the account's models are switched off, they
   * come back on and the slot takes the account's chat model (the same rule as the switch
   * itself, which refuses to go off while nothing else could answer); only with no account at
   * all does the slot fall back to dsh's stock DeepSeek default, which the next key or sign-in
   * adopts again as it does on a fresh profile. The main chat follows when it sat on that
   * route. Returns true when the account's models came back on.
   */
  private async releaseChatRoute(id: string, safeLogging = false): Promise<boolean> {
    const svc = this.defaultModelService()
    if (!svc) return false
    let cloudModelsOn = false
    try {
      let to: { provider: string; model: string } | undefined
      if (svc.currentSelection().provider === id) {
        let next = this.chatOptions().find((o) => o.provider !== id)
        if (!next && this.state.cloudModelsOff && this.signedInCache && this.state.account) {
          const { cloudModelsOff: _off, ...rest } = this.state
          this.state = rest
          next = this.chatOptions().find((o) => o.provider !== id)
          cloudModelsOn = true
          this.ctx.logger.info('nanomuse: %s was the last chat source; the account\'s models are on again', id)
        }
        to = next ? { provider: next.provider, model: next.id } : { ...STOCK_DEFAULT_MODEL }
        await svc.saveSelection(to)
        this.ctx.logger.info('nanomuse: %s is gone; new chats answer through %s/%s', id, to.provider, to.model)
      }
      const main = this.syncMainSession()
      if (main) {
        const current = await this.sessionSelection(main).catch(() => undefined)
        const held = this.cloudOnce.get(main)
        if (current?.provider === id || held?.provider === id) {
          to ??= svc.currentSelection()
          await this.mainChatFollows(to.provider, to.model)
        }
      }
    } catch (error: unknown) {
      if (safeLogging) this.ctx.logger.warn('nanomuse: the chat slot could not leave an old own-provider route; cleanup will retry on the next start')
      else this.ctx.logger.warn('nanomuse: the chat slot could not leave %s: %s', id, message(error))
    }
    return cloudModelsOn
  }

  /** `POST /chatgpt/login`: starts the runtime's sign-in and returns the page to open; `done` follows in the live state. */
  chatGptLogin(): Promise<string> {
    return this.chatgpt.beginLogin()
  }

  /** The proxy came up: the `chatgpt` row — the loopback URL, the local token as a credential, the models it lists, chat and vision. */
  private async chatGptReady(url: string, token: string, models: string[]): Promise<void> {
    await this.ctx.credentials.set(credentialRef(CHATGPT_KEY_REF), token)
    const entry = this.catalogue.find((p) => p.id === 'openai')
    const capabilities: Capability[] = entry ? capabilitiesForAuth(entry, 'oauth-chatgpt').filter((c) => c !== 'image' && c !== 'video') : ['chat', 'vision']
    const listed = models.length ? models : await listModels('openai', url, token)
    const ids = listed.length ? listed : ['gpt-6.1-sol', 'gpt-6-sol', 'gpt-6-luna', 'gpt-6-astra', 'gpt-5.6-sol', 'gpt-5.4', 'gpt-5.4-mini']
    const label = this.chatgpt.login.label || this.state.chatgpt?.label || 'ChatGPT'
    const own: OwnModel[] = ids.map((id) => ({ id, name: id, vision: capabilities.includes('vision'), kind: 'chat' as const }))
    const row: OwnProvider = { id: CHATGPT_PROVIDER, provider: CHATGPT_PROVIDER, label, protocol: 'openai', baseURL: url, keyRef: CHATGPT_KEY_REF, capabilities, models: own, at: Date.now() }
    await this.ctx.settings.update(LLM_ROW, { providers: { [CHATGPT_PROVIDER]: ownProviderRow(row) } })
    const fresh = !this.state.chatgpt
    this.state = { ...this.state, providers: { ...this.state.providers, [CHATGPT_PROVIDER]: row }, chatgpt: { label, at: this.state.chatgpt?.at ?? Date.now() } }
    await this.writeState()
    if (fresh) await this.adoptOwnDefault(CHATGPT_PROVIDER, row)
    await this.writeHands()
    this.broadcast()
  }

  /** The runtime says the sign-in is gone: the row goes with it. */
  private async chatGptGone(): Promise<void> {
    const row = this.state.providers?.[CHATGPT_PROVIDER]
    if (row) await this.dropOwnRow(CHATGPT_PROVIDER, row)
    delete this.state.chatgpt
    await this.writeState()
    this.broadcast()
  }

  /** `POST /chatgpt/logout`: the runtime forgets the tokens, the proxy stops, the row goes. */
  async chatGptLogout(): Promise<void> {
    await this.chatgpt.logout()
    await this.chatGptGone()
  }

  /** The chat model new chats answer through and where it lives: the account, an own row, or something else of dsh's (`provider` then names it, `model` empty). */
  /**
   * The ids this computer runs its chats under, for `hello` (relay 0.28): `cloud` and the
   * model when the chat slot is the account's models; the catalogue id of the own row and
   * its model when it is a key of the person's own; nothing when the slot is unset or names
   * a provider this plugin does not know. Plain ids only, never the URL or the key.
   */
  private helloLlm(): { provider: string; model: string } | undefined {
    const chat = this.chatChoice()
    if (!chat.provider || !chat.model) return undefined
    if (chat.provider === PROVIDER_ID) return helloLlm('cloud', chat.model)
    const row = this.state.providers?.[chat.provider]
    return row ? helloLlm(row.provider, chat.model) : undefined
  }

  chatChoice(): { provider: string; model: string } {
    try {
      const current = this.defaultModelService()?.currentSelection()
      if (!current) return { provider: '', model: '' }
      if (current.provider === PROVIDER_ID || this.state.providers?.[current.provider]) return current
      return { provider: current.provider, model: '' }
    } catch {
      return { provider: '', model: '' }
    }
  }

  // ---- connectors shared across devices (C3) ---------------------------------------

  /**
   * The `device` of a profile write: this device's stable id, as the hub's hello says it
   * (contract C3) — the relay replaces only the connectors whose `device_id` is the writer's
   * and refuses entries naming another device, so the two must be the same string.
   */
  private writerId(): string {
    return this.state.deviceId || this.deviceName()
  }

  /** Write this device's connections to the account's profile (ids, labels, where; never a credential). */
  private async publishConnectors(): Promise<void> {
    const connectors = (this.ctx as unknown as { get(name: string): unknown }).get('nanomuseConnectors') as { view(): { connections: Parameters<typeof sharedConnectors>[0] } } | undefined
    const token = await this.token()
    if (!connectors || !token || !this.state.account) return
    const rows = sharedConnectors(connectors.view().connections, this.deviceName(), this.state.deviceId ?? '')
    const key = JSON.stringify(rows.map(({ at: _at, ...rest }) => rest))
    if (key === this.lastSharedConnectors) return
    // Only the connectors: the relay keeps the look as it is for a write that carries nothing
    // else. (0.1.36 sent the look this device wore too, so a desktop that had just signed in,
    // still wearing the default, renamed the account's muse back to "nanoMuse" and took a drawn
    // face off every device before its first pull landed.)
    await this.relay.putConnectors(token, rows, this.state.deviceId ?? '', this.writerId())
    this.lastSharedConnectors = key
    await this.profile.pull(token, true).catch(() => undefined)
  }

  /** The relay this service talks to. */
  get baseURL(): string {
    return this.relay.origin
  }

  /** Signed in, as far as the last check went (no credential read). */
  get signedIn(): boolean {
    return this.signedInCache
  }

  async status(): Promise<CloudStatus> {
    const token = await this.token()
    const signedIn = Boolean(token && this.state.account)
    this.signedInCache = signedIn
    return {
      signedIn,
      ready: signedIn || (await this.otherProviderReady()),
      baseURL: this.relay.origin,
      ...(this.state.account ? { account: this.state.account } : {}),
      models: this.state.models ?? [],
      chatModel: this.chatModel(),
      handsModel: this.handsModel(),
      profile: this.profile.current(),
      hub: this.hubState(),
    }
  }

  async mordeHubStatus(): Promise<MordeHubStatus> {
    const credential = await this.credential(MORDE_CLIENT_CREDENTIAL)
    const state = this.state.mordeHub
    this.mordeHubHasCredential = Boolean(credential && state?.installationID)
    if (credential && state?.installationID) {
      try {
        this.mordeHubDevices = await this.loadMordeHubDevices(state.baseURL, credential)
        this.mordeHubDeviceError = undefined
      } catch (error) {
        this.mordeHubDeviceError = message(error)
      }
    } else this.mordeHubDevices = []
    await this.refreshMordeHubCapabilities()
    return this.mordeHubView()
  }

  /** Read one owner-scoped account object through Morde's Personal application API. */
  async mordePersonalObject(objectType: string, objectID: string): Promise<MordePersonalObject | undefined> {
    const { baseURL, credential } = await this.mordeAPIAuth()
    try {
      const value = await mordeRequest<unknown>(baseURL, `personal-data/${mordeObjectSegment(objectType)}/${mordeObjectSegment(objectID)}`, {
        headers: { authorization: `Bearer ${credential}` },
      })
      return mordePersonalObjectOf(value)
    } catch (error) {
      if (error instanceof RelayError && error.status === 404 && error.code === 'not_found') return undefined
      throw error
    }
  }

  /** Replace one Personal object with the caller's saved revision and idempotency key. */
  async mordeReplacePersonalObject(objectType: string, objectID: string, baseRevision: number, body: Record<string, unknown>, mutationID: string): Promise<MordePersonalMutationReceipt> {
    const { baseURL, credential } = await this.mordeAPIAuth()
    validateMordeMutation(baseRevision, mutationID)
    return mordeMutationReceiptOf(await mordeRequest<unknown>(baseURL, `personal-data/${mordeObjectSegment(objectType)}/${mordeObjectSegment(objectID)}`, {
      method: 'PUT',
      headers: { authorization: `Bearer ${credential}`, 'content-type': 'application/json', 'idempotency-key': mutationID },
      body: JSON.stringify({ base_revision: baseRevision, body }),
    }))
  }

  async mordeDeletePersonalObject(objectType: string, objectID: string, baseRevision: number, mutationID: string): Promise<MordePersonalMutationReceipt> {
    const { baseURL, credential } = await this.mordeAPIAuth()
    validateMordeMutation(baseRevision, mutationID)
    return mordeMutationReceiptOf(await mordeRequest<unknown>(baseURL, `personal-data/${mordeObjectSegment(objectType)}/${mordeObjectSegment(objectID)}`, {
      method: 'DELETE',
      headers: { authorization: `Bearer ${credential}`, 'content-type': 'application/json', 'idempotency-key': mutationID },
      body: JSON.stringify({ base_revision: baseRevision }),
    }))
  }

  /** Read the account's bounded Personal snapshot pages, including memory and profile objects. */
  async mordePersonalSnapshot(cursor = ''): Promise<MordePersonalSnapshotPage> {
    const { baseURL, credential } = await this.mordeAPIAuth()
    const query = new URLSearchParams({ limit: '10', ...(cursor ? { cursor } : {}) })
    return mordePersonalSnapshotOf(await mordeRequest<unknown>(baseURL, `sync/snapshot?${query}`, {
      headers: { authorization: `Bearer ${credential}` },
    }, 12 * 1024 * 1024))
  }

  /** List account goals through GoalService, preserving its cursor and typed business boundary. */
  async mordeGoals(cursor = ''): Promise<MordeGoalPage> {
    const { baseURL, credential } = await this.mordeAPIAuth()
    const query = new URLSearchParams({ limit: '100', ...(cursor ? { cursor } : {}) })
    return mordeGoalPageOf(await mordeRequest<unknown>(baseURL, `goals?${query}`, {
      headers: { authorization: `Bearer ${credential}` },
    }))
  }

  async mordeAvailableModels(expectedScope?: string): Promise<MordeAvailableModel[]> {
    if (expectedScope && this.mordePersonalScope() !== expectedScope) throw new RelayError(401, 'signed_out', 'The Morde account changed; try again')
    const { baseURL, credential } = await this.mordeAPIAuth()
    if (expectedScope && this.mordePersonalScope() !== expectedScope) throw new RelayError(401, 'signed_out', 'The Morde account changed; try again')
    return mordeAvailableModelsOf(await mordeRequest<unknown>(baseURL, 'available-models', {
      headers: { authorization: `Bearer ${credential}` },
    }, 256 * 1024))
  }

  async mordeCreateSchedule(input: MordeScheduleCreateInput, idempotencyKey: string, expectedScope?: string): Promise<MordeScheduleReceipt> {
    if (expectedScope && this.mordePersonalScope() !== expectedScope) throw new RelayError(401, 'signed_out', 'The Morde account changed; try again')
    const { baseURL, credential } = await this.mordeAPIAuth()
    if (expectedScope && this.mordePersonalScope() !== expectedScope) throw new RelayError(401, 'signed_out', 'The Morde account changed; try again')
    validateMordeMutation(0, idempotencyKey)
    return mordeScheduleReceiptOf(await mordeRequest<unknown>(baseURL, 'schedules', {
      method: 'POST',
      headers: { authorization: `Bearer ${credential}`, 'content-type': 'application/json', 'idempotency-key': idempotencyKey },
      body: JSON.stringify(input),
    }))
  }

  async mordeCreateGoal(id: string, body: Record<string, unknown>, mutationID: string): Promise<MordePersonalMutationReceipt> {
    const { baseURL, credential } = await this.mordeAPIAuth()
    mordeObjectSegment(id)
    validateMordeMutation(0, mutationID)
    return mordeMutationReceiptOf(await mordeRequest<unknown>(baseURL, 'goals', {
      method: 'POST',
      headers: { authorization: `Bearer ${credential}`, 'content-type': 'application/json', 'idempotency-key': mutationID },
      body: JSON.stringify({ id, body }),
    }))
  }

  async mordeUpdateGoal(id: string, baseRevision: number, body: Record<string, unknown>, mutationID: string): Promise<MordePersonalMutationReceipt> {
    const { baseURL, credential } = await this.mordeAPIAuth()
    validateMordeMutation(baseRevision, mutationID)
    return mordeMutationReceiptOf(await mordeRequest<unknown>(baseURL, `goals/${mordeObjectSegment(id)}`, {
      method: 'PATCH',
      headers: { authorization: `Bearer ${credential}`, 'content-type': 'application/json', 'idempotency-key': mutationID },
      body: JSON.stringify({ base_revision: baseRevision, body }),
    }))
  }

  async mordeDeleteGoal(id: string, baseRevision: number, mutationID: string): Promise<MordePersonalMutationReceipt> {
    const { baseURL, credential } = await this.mordeAPIAuth()
    validateMordeMutation(baseRevision, mutationID)
    return mordeMutationReceiptOf(await mordeRequest<unknown>(baseURL, `goals/${mordeObjectSegment(id)}`, {
      method: 'DELETE',
      headers: { authorization: `Bearer ${credential}`, 'content-type': 'application/json', 'idempotency-key': mutationID },
      body: JSON.stringify({ base_revision: baseRevision }),
    }))
  }

  private async mordeAPIAuth(): Promise<{ baseURL: string; credential: string }> {
    const state = this.state.mordeHub
    const credential = await this.credential(MORDE_CLIENT_CREDENTIAL)
    if (!state?.installationID || !credential) throw new RelayError(401, 'signed_out', 'Sign in to Morde first')
    return { baseURL: state.baseURL, credential }
  }

  private async loadMordeHubDevices(baseURL: string, credential: string): Promise<MordeHubDevice[]> {
    const value = await mordeRequest<unknown>(baseURL, 'devices', { headers: { authorization: `Bearer ${credential}` } })
    return mordeHubDevicesOf(value)
  }

  private async refreshMordeHubDevicesFromHub(hub: HubClient): Promise<void> {
    const state = this.state.mordeHub
    if (!state?.installationID || this.mordeHub !== hub) return
    try {
      const credential = await this.credential(MORDE_CLIENT_CREDENTIAL)
      if (!credential) return
      this.mordeHubDevices = await this.loadMordeHubDevices(state.baseURL, credential)
      this.mordeHubDeviceError = undefined
    } catch (error) {
      if (this.mordeHub !== hub) return
      this.mordeHubDeviceError = message(error)
    }
    if (this.mordeHub === hub && this.state.mordeHub === state) this.broadcast()
  }

  async mordeHubRevokeDevice(input: unknown): Promise<MordeHubStatus> {
    return this.serialize(async () => {
      if (typeof input !== 'string' || !/^i[a-z0-9]{7}$/.test(input)) {
        throw new RelayError(400, 'invalid_device', 'Choose a valid device')
      }
      const state = this.state.mordeHub
      const credential = await this.credential(MORDE_CLIENT_CREDENTIAL)
      if (!state?.installationID || !credential) throw new RelayError(401, 'signed_out', 'Sign in to Morde Hub first')
      if (input === state.installationID) throw new RelayError(400, 'current_device', 'Use Disconnect to sign out this computer')
      const target = this.mordeHubDevices.find((device) => device.id === input)
      if (!target || target.revoked || Date.parse(target.expiresAt) <= Date.now()) {
        throw new RelayError(409, 'device_inactive', 'Refresh the device list; this installation is no longer active')
      }
      try {
        await mordeRequest<void>(state.baseURL, `devices/${input}`, {
          method: 'DELETE', headers: { authorization: `Bearer ${credential}` },
        })
      } catch {
        try {
          this.mordeHubDevices = await this.loadMordeHubDevices(state.baseURL, credential)
          const current = this.mordeHubDevices.find((device) => device.id === input)
          if (current && !current.revoked && Date.parse(current.expiresAt) > Date.now()) {
            throw new RelayError(409, 'device_revoke_not_confirmed', 'The server still lists this device as active')
          }
        } catch (error) {
          if (error instanceof RelayError && error.code === 'device_revoke_not_confirmed') throw error
          throw new RelayError(503, 'device_revoke_unknown', 'The revocation result is unknown. Refresh the device list before retrying')
        }
      }
      this.mordeHubDevices = this.mordeHubDevices.map((device) => device.id === input ? { ...device, online: false, revoked: true } : device)
      this.mordeHub?.refreshDevices()
      try { this.mordeHubDevices = await this.loadMordeHubDevices(state.baseURL, credential) }
      catch { /* The 204 response already confirmed revocation. */ }
      this.broadcast()
      return this.mordeHubView()
    })
  }

  private async refreshMordeHubCapabilities(): Promise<void> {
    const [screenshot, files, command] = await Promise.all([
      mordeHubScreenshotAvailable(),
      mordeHubFilesAvailable(),
      mordeHubCommandAvailable(),
    ])
    this.mordeHubCapabilities = { screenshot, files, command }
  }

  async mordeHubUpdateGrants(input: unknown): Promise<MordeHubStatus> {
    return this.serialize(async () => {
      if (!input || typeof input !== 'object' || Array.isArray(input)) throw new RelayError(400, 'invalid_grant', 'Choose valid Hub permissions')
      const body = input as Record<string, unknown>
      if (Object.keys(body).some((key) => key !== 'screenshot' && key !== 'files' && key !== 'command' && key !== 'file_root') ||
        ('screenshot' in body && typeof body.screenshot !== 'boolean') || ('files' in body && typeof body.files !== 'boolean') ||
        ('command' in body && typeof body.command !== 'boolean')) {
        throw new RelayError(400, 'invalid_grant', 'Choose valid Hub permissions')
      }
      const current = this.state.mordeHub
      if (!current?.installationID || !(await this.credential(MORDE_CLIENT_CREDENTIAL))) {
        throw new RelayError(401, 'signed_out', 'Sign in to Morde Hub first')
      }
      this.mordeHubHasCredential = true
      await this.refreshMordeHubCapabilities()
      let screenshot = current.grants.screenshot === true
      let files = current.grants.files === true
      let command = current.grants.command === true
      let fileRoot = current.fileRoot
      if ('screenshot' in body) {
        screenshot = body.screenshot as boolean
        if (screenshot && !this.mordeHubCapabilities.screenshot) {
          throw new RelayError(409, 'capability_unavailable', 'Screen capture is not available or its system permission is off')
        }
      }
      if ('files' in body && body.files === false) {
        files = false
      }
      if ('file_root' in body) {
        if (body.file_root === null) {
          fileRoot = undefined
          files = false
          command = false
        } else if (typeof body.file_root !== 'string' || !(this.mordeHubCapabilities.files || this.mordeHubCapabilities.command)) {
          throw new RelayError(409, 'capability_unavailable', 'The local file helper is unavailable')
        } else {
          fileRoot = await mordeHubSelectedRoot(body.file_root)
        }
      }
      if (body.files === true) {
        if (!fileRoot || !this.mordeHubCapabilities.files) {
          throw new RelayError(400, 'invalid_grant', 'Choose a folder before allowing file access')
        }
        files = true
      }
      if ('command' in body) {
        command = body.command as boolean
        if (command && (!fileRoot || !this.mordeHubCapabilities.command)) {
          throw new RelayError(409, 'capability_unavailable', 'Choose a folder and verify command isolation before allowing commands')
        }
      }
      const changed = screenshot !== current.grants.screenshot || files !== current.grants.files || command !== current.grants.command ||
        fileRoot?.directory !== current.fileRoot?.directory || fileRoot?.identity !== current.fileRoot?.identity
      if (!changed) return this.mordeHubView()
      if (current.targetGrantVersion >= Number.MAX_SAFE_INTEGER) throw new RelayError(409, 'grant_version_exhausted', 'Morde Hub permissions need to be reset')
      const nextHub = {
        ...current,
        targetGrantVersion: current.targetGrantVersion + 1,
        grants: { screenshot, files, command },
      }
      if (fileRoot) nextHub.fileRoot = fileRoot
      else delete nextHub.fileRoot
      this.state = { ...this.state, mordeHub: nextHub }
      await this.writeState()
      this.mordeHub?.restart()
      this.broadcast()
      return this.mordeHubView()
    })
  }

  mordeHubView(): MordeHubStatus {
    const state = this.state.mordeHub
    const lastError = this.mordeHub?.lastError || this.mordeHubDeviceError
    return {
      signedIn: this.mordeHubHasCredential,
      connected: this.mordeHub?.connected ?? false,
      baseURL: state?.baseURL ?? '',
      ...(state?.installationID ? { deviceID: this.mordeHub?.deviceId ?? state.installationID } : {}),
      devices: this.mordeHubDevices,
      targetGrantVersion: state?.targetGrantVersion ?? 1,
      grants: {
        screenshot: state?.grants.screenshot ?? false,
        files: state?.grants.files ?? false,
        command: state?.grants.command ?? false,
        fileRoot: state?.fileRoot?.directory ?? null,
      },
      capabilities: { ...this.mordeHubCapabilities },
      ...(state?.accountName ? { accountName: state.accountName } : {}),
      ...(state?.installationID ? { installationID: state.installationID } : {}),
      ...(state?.expiresAt ? { expiresAt: state.expiresAt } : {}),
      ...(lastError ? { lastError } : {}),
    }
  }

  /** Opaque local partition for account-owned DSH cache files; contains no credential or raw user id. */
  mordePersonalScope(): string {
    const state = this.state.mordeHub
    if (!this.mordeHubHasCredential || !state?.installationID) return ''
    const owner = state.accountID || state.installationID
    return `morde-${createHash('sha256').update(`${state.baseURL}\0${owner}`).digest('hex').slice(0, 24)}`
  }

  async mordeHubCall(to: string, action: 'open' | 'notify', args: Record<string, unknown>, options: CallOptions = {}): Promise<Record<string, unknown>> {
    if (typeof to !== 'string' || !/^i[a-z0-9]{7}$/.test(to) || !args || typeof args !== 'object' || Array.isArray(args)) {
      throw new HubError('not_allowed', 'The Morde Hub call is invalid')
    }
    if (action === 'open') {
      if (Object.keys(args).length !== 1 || !Object.hasOwn(args, 'url') || typeof args.url !== 'string') {
        throw new HubError('not_allowed', 'The Morde Hub call is invalid')
      }
      const raw = args.url
      let url: URL
      try { url = new URL(raw) }
      catch { throw new HubError('not_allowed', 'Only HTTP(S) URLs can be opened') }
      if (raw !== raw.trim() || (url.protocol !== 'http:' && url.protocol !== 'https:') || !url.hostname || url.username || url.password || Buffer.byteLength(raw) > 2048) {
        throw new HubError('not_allowed', 'Only HTTP(S) URLs can be opened')
      }
    } else if (action === 'notify') {
      if (Object.keys(args).some((key) => key !== 'title' && key !== 'text') || !Object.hasOwn(args, 'text') || typeof args.text !== 'string' || !args.text ||
        Buffer.byteLength(args.text) > 2000 || (Object.hasOwn(args, 'title') && args.title !== undefined && (typeof args.title !== 'string' || Buffer.byteLength(args.title) > 100))) {
        throw new HubError('not_allowed', 'The Morde Hub notification is invalid')
      }
    } else {
      throw new HubError('not_allowed', 'The Morde Hub call is invalid')
    }

    const state = this.state.mordeHub
    const hub = this.mordeHub
    if (!state?.installationID || !(await this.credential(MORDE_CLIENT_CREDENTIAL))) {
      throw new HubError('not_allowed', 'Morde Hub is signed out on this computer')
    }
    if (!hub?.connected || hub.deviceId !== state.installationID) {
      throw new HubError('offline', 'This computer is not connected to Morde Hub')
    }
    if (to === state.installationID) throw new HubError('not_allowed', 'Choose another Morde Hub device')
    const device = this.mordeHubDevices.find((item) => item.id === to)
    const expiresAt = device ? Date.parse(device.expiresAt) : NaN
    if (!device || device.revoked || !Number.isFinite(expiresAt) || expiresAt <= Date.now()) {
      throw new HubError('not_allowed', 'Choose an active Morde Hub device')
    }
    if (!device.online) throw new HubError('offline', 'The Morde Hub device is offline')
    if (action === 'open' && device.platform !== 'desktop') throw new HubError('not_allowed', 'URLs can be opened only on Morde Hub desktop devices')
    if (action === 'notify' && device.platform !== 'desktop') throw new HubError('not_allowed', 'Notifications are available only on Morde Hub desktop devices')
    const request = action === 'open'
      ? { url: args.url as string }
      : { text: args.text as string, ...(Object.hasOwn(args, 'title') && typeof args.title === 'string' ? { title: args.title } : {}) }
    return hub.call(to, action, request, options)
  }

  async mordeHubLogin(baseURLInput: string, nameInput: string, password: string): Promise<MordeHubStatus> {
    return this.serialize(async () => {
      if (await this.credential(MORDE_CLIENT_CREDENTIAL)) throw new RelayError(409, 'already_signed_in', 'Disconnect the current Morde installation first')
      const baseURL = mordeBaseURL(baseURLInput)
      const name = nameInput.trim()
      if (!name || !password || Buffer.byteLength(password) > 4096) throw new RelayError(400, 'invalid_login', 'Enter a user name and password')
      const capabilities = await mordeRequest<{ supported_features?: unknown }>(baseURL, 'capabilities')
      const features = Array.isArray(capabilities.supported_features) ? capabilities.supported_features : []
      if (!features.includes('ordered_client_login') || !features.includes('hub')) {
        throw new RelayError(409, 'hub_unavailable', 'This Morde server does not support ordered client login and Hub')
      }

      const previous = this.state.mordeHub
      const retrying = previous?.baseURL === baseURL && !previous.installationID
      const clientInstanceID = retrying && previous.clientInstanceID ? previous.clientInstanceID : randomUUID()
      const attemptGeneration = retrying ? previous.attemptGeneration + 1 : 1
      this.state = {
        ...this.state,
        mordeHub: { baseURL, clientInstanceID, attemptGeneration, targetGrantVersion: 1, grants: { screenshot: false, files: false, command: false } },
      }
      await this.writeState()
      const result = await mordeRequest<{
        installation_id?: unknown
        client_credential?: unknown
        client_expires_at?: unknown
        user?: { id?: unknown; name?: unknown }
      }>(baseURL, 'auth/client-login', {
        method: 'POST',
        headers: { 'content-type': 'application/json', 'idempotency-key': randomUUID() },
        body: JSON.stringify({
          name,
          password,
          platform: 'desktop',
          display_name: this.deviceName().slice(0, 100),
          client_instance_id: clientInstanceID,
          attempt_generation: attemptGeneration,
        }),
      })
      if (typeof result.client_credential !== 'string' || !result.client_credential.startsWith('chc_') ||
          typeof result.installation_id !== 'string' || !result.installation_id || result.installation_id.length > 128) {
        throw new RelayError(502, 'bad_response', 'Morde server did not return a Client Installation credential')
      }
      const expiry = typeof result.client_expires_at === 'string' && Number.isFinite(Date.parse(result.client_expires_at))
        ? new Date(result.client_expires_at).toISOString()
        : undefined
      await this.ctx.credentials.set(credentialRef(MORDE_CLIENT_CREDENTIAL), result.client_credential)
      this.mordeHubHasCredential = true
      this.mordeHubDeviceError = undefined
      this.state = {
        ...this.state,
        mordeHub: {
          baseURL,
          clientInstanceID,
          attemptGeneration,
          targetGrantVersion: 1,
          grants: { screenshot: false, files: false, command: false },
          ...(typeof result.user?.id === 'string' ? { accountID: result.user.id.slice(0, 128) } : {}),
          accountName: typeof result.user?.name === 'string' ? result.user.name.slice(0, 100) : name.slice(0, 100),
          installationID: result.installation_id,
          ...(expiry ? { expiresAt: expiry } : {}),
        },
      }
      await this.writeState()
      this.startMordeHub()
      await this.refreshMordeHubCapabilities()
      this.broadcast()
      return this.mordeHubView()
    })
  }

  async mordeHubSignOut(): Promise<MordeHubStatus> {
    return this.serialize(async () => {
      const credential = await this.credential(MORDE_CLIENT_CREDENTIAL)
      const baseURL = this.state.mordeHub?.baseURL
      if (credential && baseURL) {
        try {
          await mordeRequest<void>(baseURL, 'auth/client-logout', {
            method: 'POST',
            headers: { authorization: `Bearer ${credential}` },
          })
        } catch (error: unknown) {
          if (!(error instanceof RelayError && error.status === 401)) throw error
        }
      }
      this.mordeHub?.stop('signed out')
      this.mordeHub = undefined
      await this.ctx.credentials.unset(credentialRef(MORDE_CLIENT_CREDENTIAL))
      this.mordeHubHasCredential = false
      this.mordeHubCapabilities = { screenshot: false, files: false, command: false }
      this.mordeHubDevices = []
      this.mordeHubDeviceError = undefined
      const { mordeHub: _mordeHub, ...state } = this.state
      this.state = state
      await this.writeState()
      this.broadcast()
      return this.mordeHubView()
    })
  }

  private startMordeHub(): void {
    const state = this.state.mordeHub
    if (!state?.installationID) return
    this.mordeHub?.stop('replaced')
    const hub = new HubClient({
      url: mordeHubURL(state.baseURL),
      auth: 'bearer',
      key: async () => (await this.credential(MORDE_CLIENT_CREDENTIAL)) || undefined,
      log: (level, text) => this.ctx.logger[level](text),
    })
    hub.handle('info', async () => {
      const bounded = (value: string, bytes: number) => Buffer.from(value).subarray(0, bytes).toString('utf8')
      await this.refreshMordeHubCapabilities()
      const state = this.state.mordeHub
      const actions = ['info', 'open', 'notify']
      if (state?.grants.screenshot && this.mordeHubCapabilities.screenshot) actions.push('screenshot')
      if (state?.grants.files && state.fileRoot && this.mordeHubCapabilities.files) actions.push('files', 'file.get')
      if (state?.grants.command && state.fileRoot && this.mordeHubCapabilities.command) actions.push('command')
      return {
        name: bounded(this.deviceName(), 100),
        platform: 'desktop',
        os: bounded(`${type()} ${release()}`.trim(), 64),
        version: bounded(VERSION, 64),
        actions,
        target_grant_version: state?.targetGrantVersion ?? 1,
      }
    })
    hub.handle('open', async (args, call) => this.openMordeHub(args, call.from, call.signal))
    hub.handle('notify', async (args, call) => {
      const title = typeof args.title === 'string' ? args.title : ''
      const text = typeof args.text === 'string' ? args.text : ''
      if (!text || Buffer.byteLength(title) > 100 || Buffer.byteLength(text) > 2000) {
        throw new HubError('not_allowed', 'The notification is invalid')
      }
      this.notice('notify', call.from.name || 'a device', title, text)
      return { notified: true }
    })
    hub.handle('screenshot', async (args, call) => {
      if (Object.keys(args).length !== 0) throw new HubError('not_allowed', 'Screenshot does not accept arguments')
      const { targetGrantVersion } = await this.mordeHubTaskGrant('screenshot', call.from)
      const result = await mordeHubScreenshot(targetGrantVersion)
      await this.recheckMordeHubTaskGrant('screenshot', call.from)
      return result
    })
    hub.handle('files', async (args, call) => {
      if (Object.keys(args).some((key) => key !== 'path')) throw new HubError('not_allowed', 'The folder request is invalid')
      const { state, targetGrantVersion } = await this.mordeHubTaskGrant('files', call.from)
      if (!state.fileRoot) throw new HubError('not_allowed', 'File access is not allowed on this computer')
      const result = await mordeHubFiles(state.fileRoot, args.path, targetGrantVersion)
      await this.recheckMordeHubTaskGrant('files', call.from)
      return result
    })
    hub.handle('file.get', async (args, call) => {
      if (Object.keys(args).some((key) => key !== 'path' && key !== 'max_bytes') ||
        typeof args.path !== 'string' || !Number.isSafeInteger(args.max_bytes)) {
        throw new HubError('not_allowed', 'The file request is invalid')
      }
      const { state, targetGrantVersion } = await this.mordeHubTaskGrant('file.get', call.from)
      if (!state.fileRoot) throw new HubError('not_allowed', 'File access is not allowed on this computer')
      const result = await mordeHubFileGet(state.fileRoot, args.path, args.max_bytes, targetGrantVersion)
      await this.recheckMordeHubTaskGrant('file.get', call.from)
      return result
    })
    hub.handle('command', async (args, call) => {
      if (Object.keys(args).some((key) => key !== 'command' && key !== 'timeout_ms') ||
        typeof args.command !== 'string' || Buffer.byteLength(args.command) < 1 || Buffer.byteLength(args.command) > 8192 ||
        args.command.includes('\0') || !Number.isSafeInteger(args.timeout_ms) || Number(args.timeout_ms) < 1000 || Number(args.timeout_ms) > 300000) {
        throw new HubError('not_allowed', 'The command request is invalid')
      }
      const { state, targetGrantVersion } = await this.mordeHubTaskGrant('command', call.from)
      if (!state.fileRoot) throw new HubError('not_allowed', 'Command access is not allowed on this computer')
      const result = await mordeHubCommand(state.fileRoot, args.command, Number(args.timeout_ms), targetGrantVersion, call.signal)
      await this.recheckMordeHubTaskGrant('command', call.from)
      return result
    })
    this.mordeHub = hub
    hub.onState(() => this.broadcast())
    hub.onDevices(() => void this.refreshMordeHubDevicesFromHub(hub))
    hub.onUnauthorized(() => {
      void this.serialize(() => this.clearMordeHubAuthorization(hub)).catch((error: unknown) =>
        this.ctx.logger.warn('nanomuse: Morde Hub credentials not cleared after revocation: %s', message(error)))
    })
    hub.start()
  }

  private async clearMordeHubAuthorization(hub: HubClient): Promise<void> {
    if (this.mordeHub !== hub) return
    hub.stop('authorization revoked')
    this.mordeHub = undefined
    await this.ctx.credentials.unset(credentialRef(MORDE_CLIENT_CREDENTIAL))
    this.mordeHubHasCredential = false
    this.mordeHubCapabilities = { screenshot: false, files: false, command: false }
    this.mordeHubDevices = []
    this.mordeHubDeviceError = 'The Morde Hub authorization was revoked. Sign in again to reconnect.'
    const { mordeHub: _mordeHub, ...state } = this.state
    this.state = state
    await this.writeState()
    this.broadcast()
  }

  private async mordeHubTaskGrant(action: 'screenshot' | 'files' | 'file.get' | 'command', caller: Caller): Promise<{ state: MordeHubState; targetGrantVersion: number }> {
    const state = this.state.mordeHub
    if (!state?.installationID || !(await this.credential(MORDE_CLIENT_CREDENTIAL))) {
      throw new HubError('not_allowed', 'Morde Hub is signed out on this computer')
    }
    if (action === 'command' && !this.mordeHubCapabilities.command) {
      throw new HubError('unsupported', 'Command isolation is unavailable on this computer')
    }
    const targetGrantVersion = requireMordeHubTaskGrant(action, caller, state)
    return { state, targetGrantVersion }
  }

  private async recheckMordeHubTaskGrant(action: 'screenshot' | 'files' | 'file.get' | 'command', caller: Caller): Promise<void> {
    const state = this.state.mordeHub
    if (!state?.installationID || !(await this.credential(MORDE_CLIENT_CREDENTIAL))) {
      throw new HubError('not_allowed', 'Morde Hub is signed out on this computer')
    }
    requireMordeHubTaskGrant(action, caller, state)
  }

  private async openMordeHub(args: Record<string, unknown>, from: Caller, signal?: AbortSignal): Promise<Record<string, unknown>> {
    const raw = typeof args.url === 'string' ? args.url : ''
    let url: URL
    try {
      url = new URL(raw)
    } catch {
      throw new HubError('not_allowed', 'Only HTTP(S) URLs can be opened')
    }
    if ((url.protocol !== 'http:' && url.protocol !== 'https:') || !url.hostname || url.username || url.password || Buffer.byteLength(raw) > 2048) {
      throw new HubError('not_allowed', 'Only HTTP(S) URLs can be opened')
    }
    const answer = await this.asks.ask(from, 'open', raw, ASK_TIMEOUT_MS, true, signal)
    if (answer === 'cancelled') throw signal?.reason instanceof Error ? signal.reason : new HubError('cancelled', 'The Hub action was cancelled')
    if (answer !== 'once') {
      throw new HubError('not_allowed', answer === 'timeout' ? `nobody at ${this.deviceName()} answered in time` : `the person at ${this.deviceName()} did not allow it`)
    }
    const result = await runAction('open', { url: raw })
    if (result.ok !== true) throw new HubError('failed', 'The URL could not be opened')
    return { opened: true }
  }

  private defaultModelService(): { currentSelection(): { provider: string; model: string }; saveSelection(next: { provider: string; model: string }): Promise<void> } | undefined {
    return (this.ctx as unknown as { get(name: string): unknown }).get('agentDefaultModel') as ReturnType<NanomuseCloud['defaultModelService']>
  }

  /** The account model new chats answer through, when the default is ours; empty when another provider is chosen. */
  chatModel(): string {
    try {
      const current = this.defaultModelService()?.currentSelection()
      return current?.provider === PROVIDER_ID ? current.model : ''
    } catch {
      return ''
    }
  }

  /**
   * Make an account chat model — or an own row's (C11) — the default for new chats (Settings →
   * Models → Chat). The hands, the pictures and the clips follow the chat provider while the
   * person has not chosen them, so the hands' environment is written again.
   */
  async setChatModel(id: string, provider = PROVIDER_ID): Promise<void> {
    if (!this.chatOptions().some((o) => o.id === id && o.provider === provider)) throw new RelayError(400, 'bad_model', 'Not a chat model of the account or of an own key')
    const svc = this.defaultModelService()
    if (!svc) throw new RelayError(503, 'no_models', 'Not available yet')
    await svc.saveSelection({ provider, model: id })
    await this.mainChatFollows(provider, id).catch((error: unknown) => this.ctx.logger.warn('nanomuse cloud: the main chat did not follow the chat model: %s', message(error)))
    await this.writeHands().catch((error: unknown) => this.ctx.logger.warn('nanomuse cloud: hands model not written: %s', message(error)))
    this.broadcast()
  }

  /**
   * The main chat follows the chat slot. Every session keeps the model it was given, and the
   * slot (`agentDefaultModel`) is the default for new ones; but the main chat is the one
   * conversation the Chat tab always shows and is never new, so on 0.1.41 the Models page could
   * not move it off the provider it started on: a person who saved a key of their own saw their
   * side chats answer through it while the main chat kept nanoMuse Cloud under the face. The
   * main chat's own selection now moves with the slot (the same `selectModel` the chat's own
   * picker makes); side chats are left as they are; nothing is written when the session already
   * says so. A main chat under *Use nanoMuse Cloud this time* finishes that turn on the account
   * and comes back to the pick. True when the session was moved (or will be after the turn).
   */
  async mainChatFollows(provider: string, model: string): Promise<boolean> {
    const main = this.syncMainSession()
    const ctx = this.sessionCtx
    if (!main || !ctx) return false
    const held = this.cloudOnce.get(main)
    if (held) {
      if (held.provider === provider && held.model === model) return false
      this.cloudOnce.set(main, { provider, model, at: held.at })
      return true
    }
    const current = await this.sessionSelection(main).catch(() => undefined)
    if (current && current.provider === provider && current.model === model) return false
    await ctx.sessionController.selectModel({ sessionId: main as SessionId, provider, model })
    this.ctx.logger.info('nanomuse: the main chat follows the chat model, %s/%s', provider, model)
    return true
  }

  /** A session's model as dsh resolves it: a pending choice, else the last request's, else the slot. */
  private async sessionSelection(sessionId: string): Promise<{ provider: string; model: string; reasoningEffort?: string } | undefined> {
    const ctx = this.sessionCtx
    if (!ctx) return undefined
    const resolved = await ctx.sessionController.resolveAgent(sessionId as SessionId)
    if ('error' in resolved) throw new RelayError(404, 'not_found', String(resolved.error))
    type Selection = { provider: string; model: string; reasoningEffort?: string }
    const session = resolved.agent.session as unknown as { requestHeader?(): { config?: Selection } | undefined }
    const projections = (ctx as unknown as { get(name: string): unknown }).get('sessionProjections') as { stateOf(session: unknown, key: string): { pending?: Selection | null } | undefined } | undefined
    const pending = projections?.stateOf(resolved.agent.session, 'modelSelection')?.pending ?? undefined
    const logged = session.requestHeader?.()?.config
    return pending ?? (logged?.provider && logged.model ? logged : this.defaultModelService()?.currentSelection())
  }

  /** The chat slot as the Models page shows it: the provider's label next to the id. */
  private labelOf(provider: string): string {
    if (provider === PROVIDER_ID) return 'nanoMuse Cloud'
    return this.state.providers?.[provider]?.label ?? provider
  }

  /** `GET /models`: the four slots, what each uses and lists (0.1.41). */
  async modelsView(): Promise<ModelsView> {
    const signedIn = this.signedInCache && Boolean(this.state.account)
    const chat = this.chatChoice()
    const hands = this.handsChoice()
    const image = await this.imageEndpoint()
    const video = await this.videoEndpoint()
    const media = this.state.media ?? {}
    const slot = (provider: string, model: string, options: ModelOption[], chosen: boolean): SlotView => ({ provider, providerLabel: provider ? this.labelOf(provider) : '', model, options, chosen })
    // what *Automatic* gives right now: the order with the stored choice set aside
    const auto = (provider: string, model: string): NonNullable<SlotView['auto']> => ({ provider, providerLabel: provider ? this.labelOf(provider) : '', model })
    const handsAuto = this.handsChoice({ auto: true })
    const imageAuto = await this.imageEndpoint({ auto: true })
    const videoAuto = await this.videoEndpoint({ auto: true })
    // `chosen`: a stored choice that is the one in use (a stale one, say of a row that went, counts as automatic)
    const handsChosen = Boolean(this.state.handsModel) && hands.model === this.state.handsModel && hands.provider === (this.state.handsProvider ?? PROVIDER_ID)
    const imageChosen = Boolean(media.imageModel) && image?.model === media.imageModel && image?.instanceId === (media.imageProvider ?? PROVIDER_ID)
    const videoChosen = Boolean(media.videoModel) && media.videoModel !== VIDEO_OFF && video?.model === media.videoModel && (!media.videoProvider || video?.instanceId === media.videoProvider)
    return {
      signedIn,
      cloudModels: this.cloudModels(),
      slots: {
        chat: slot(chat.provider, chat.model, this.chatOptions(), Boolean(chat.provider)),
        hands: { ...slot(hands.provider, hands.model, this.handsOptions(), handsChosen), auto: auto(handsAuto.provider, handsAuto.model) },
        image: { ...slot(image?.instanceId ?? '', image?.model ?? '', this.imageOptions(), imageChosen), auto: auto(imageAuto?.instanceId ?? '', imageAuto?.model ?? '') },
        video: { ...slot(video?.instanceId ?? '', video?.model ?? '', await this.videoOptions(), videoChosen), ...(media.videoModel === VIDEO_OFF ? { off: true } : {}), auto: auto(videoAuto?.instanceId ?? '', videoAuto?.model ?? '') },
      },
      handsExcluded: this.handsExcluded(),
    }
  }

  /**
   * *Use nanoMuse Cloud this time* under a failed turn (contract section 4): the session's
   * next request goes through the account's recommended chat model, and the selection it had
   * is put back when that turn ends; the chat slot (the default for new chats) is left as it
   * was, whatever dsh saved along the way. The browser half then sends the words again.
   */
  async retryOnCloud(sessionId: string): Promise<{ provider: string; model: string }> {
    const token = await this.token()
    if (!token || !this.state.account) throw new RelayError(401, 'signed_out', 'Sign in to use nanoMuse Cloud')
    const pick = pickChatModel(this.state.models ?? [])
    if (!pick) throw new RelayError(409, 'no_chat_model', 'The account lists no chat model')
    const ctx = this.sessionCtx
    if (!ctx) throw new RelayError(503, 'not_ready', 'The session API is not up yet')
    const slot = this.defaultModelService()?.currentSelection()
    const was = await this.sessionSelection(sessionId)
    const before: CloudOnce | undefined = was ? { provider: was.provider, model: was.model, ...(was.reasoningEffort ? { reasoningEffort: was.reasoningEffort } : {}), at: Date.now() } : undefined
    await ctx.sessionController.selectModel({ sessionId: sessionId as SessionId, provider: PROVIDER_ID, model: pick.id })
    // dsh saves a session's selection as the default in the background; the slot stays what it was
    if (slot) await this.defaultModelService()?.saveSelection(slot).catch(() => undefined)
    // With Cloud models off the session is held too (the `llm/stream` hook lets a held session through), whatever it was on
    if (before && (before.provider !== PROVIDER_ID || this.state.cloudModelsOff)) this.cloudOnce.set(sessionId, before)
    else if (this.state.cloudModelsOff) this.cloudOnce.set(sessionId, { provider: PROVIDER_ID, model: pick.id, at: Date.now() })
    else this.cloudOnce.delete(sessionId)
    return { provider: PROVIDER_ID, model: pick.id }
  }

  /** The turn after *Use nanoMuse Cloud this time* ended: the session goes back to its own model, the slot stays. */
  private async cloudOnceDone(sessionId: string): Promise<void> {
    const before = this.cloudOnce.get(sessionId)
    if (!before) return
    this.cloudOnce.delete(sessionId)
    const ctx = this.sessionCtx
    if (!ctx) return
    const slot = this.defaultModelService()?.currentSelection()
    try {
      await ctx.sessionController.selectModel({ sessionId: sessionId as SessionId, provider: before.provider, model: before.model, ...(before.reasoningEffort ? { reasoningEffort: before.reasoningEffort } : {}) })
    } catch (error: unknown) {
      this.ctx.logger.warn('nanomuse cloud: the session did not go back to %s/%s: %s', before.provider, before.model, message(error))
    }
    if (slot) await this.defaultModelService()?.saveSelection(slot).catch(() => undefined)
  }

  /** The live state, as `/events` streams it. */
  live(): LiveState {
    const account = this.state.account
    return {
      cloud: { signedIn: this.signedInCache && Boolean(account), hint: account?.hint ?? '' },
      profile: this.profile.current(),
      hub: this.hubState(),
      mordeHub: this.mordeHubView(),
      hands: { calls: [...this.calls.values()], steps: this.steps, run: this.screenRuns.current() },
      stage: this.stage,
      notices: this.notices,
      approvals: this.approvalDesk.list(),
      holds: this.holdDesk.list(),
      grants: this.state.grants ?? [],
      goneSchedules: this.goneSchedules,
      chatTasks: this.chatTasks,
      handsModel: this.handsModel(),
      update: this.state.update ?? null,
      motion: this.motion.view(),
      ownClip: this.ownClip.view(),
      ownClips: this.ownClip.moods(),
      blackScreenAt: this.blackScreenAt,
      sync: this.sync ? { rev: this.sync.view().rev, mainSession: this.sync.state.mainSession } : { rev: 0, mainSession: '' },
      ownKeys: {
        count: Object.keys(this.state.providers ?? {}).length,
        capabilities: this.capabilities(),
        chatgpt: { signedIn: Boolean(this.state.chatgpt), label: this.state.chatgpt?.label ?? '', proxy: Boolean(this.chatgpt.ready), login: this.chatgpt.login },
      },
      trajectory: { rev: this.trajectory.view().rev, sessions: [...new Set(this.trajectory.view().runs.map((r) => r.sessionId))] },
      headsUp: this.headsUp(),
      waiting: [...this.waits.values()].sort((a, b) => a.since - b.since),
    }
  }

  /** The hands' runs of a session (or all), without pictures: `GET /trajectory?session=`. */
  trajectoryView(sessionId?: string): TrajectoryView {
    return this.trajectory.view(sessionId)
  }

  /** The session that holds the account's main conversation (C8), '' before one is known. */
  syncMainSession(): string {
    return this.sync?.state.mainSession ?? ''
  }

  /** How many turns from the account's other devices a session here shows (C8). */
  syncRemoteLines(sessionId: string): number {
    return this.kept?.linesOf(sessionId).length ?? 0
  }

  /** The main chat named by the host (the first conversation): the session it ends up in. */
  async setSyncMain(sessionId: string): Promise<string> {
    return this.sync ? this.sync.setMain(sessionId) : sessionId
  }

  /** Step one: a code to the phone or the mailbox. */
  async requestCode(identifier: string): Promise<void> {
    await this.serialize(() => this.relay.requestCode(identifier.trim()))
  }

  /** Step two: the code for the key; wires the provider and remembers the account. A friend's invite code counts for a new account. */
  verify(identifier: string, code: string, invite = ''): Promise<CloudStatus> {
    return this.serialize(async () => this.adopt(await this.relay.verify(identifier.trim(), code, this.deviceName(), undefined, invite)))
  }

  /** The account page's calls that only pass through: nothing of them is kept here. */
  private async withToken<T>(work: (token: string) => Promise<T>): Promise<T> {
    const token = await this.token()
    if (!token || !this.state.account) throw new RelayError(401, 'signed_out', 'Sign in first')
    return work(token)
  }

  /** Every key of the account retired (this one too), or the account deleted: the local side forgets as on a sign-out. */
  private endAll(how: 'sign-out-all' | 'delete'): Promise<CloudStatus> {
    return this.serialize(async () => {
      const token = await this.token()
      if (token) {
        if (how === 'delete') await this.relay.deleteAccount(token)
        else await this.relay.signOutAll(token)
      }
      await this.forget()
      return this.status()
    })
  }

  /** The other way in: the account's password instead of a code. */
  login(identifier: string, password: string): Promise<CloudStatus> {
    return this.serialize(async () => this.adopt(await this.relay.login(identifier.trim(), password, this.deviceName())))
  }

  /** A fresh key from either way in: wire the provider, remember the account, wear its look. */
  private async adopt(signIn: SignIn): Promise<CloudStatus> {
    // C10: a different account than the one this device last held (the sync state remembers it through a
    // sign-out) — the account-scoped state starts over
    const last = this.sync?.state.accountId || this.state.sync?.accountId || ''
    const switched = Boolean(last) && last !== signIn.account.id
    await this.ctx.credentials.set(credentialRef(TOKEN_REF), signIn.apiKey)
    const models = await this.relay.models(signIn.apiKey)
    await this.writeProvider(models)
    await this.adoptDefaultModel(models)
    this.state = { ...this.state, account: signIn.account, models }
    await this.writeState()
    await this.writeHands().catch((error: unknown) => this.ctx.logger.warn('nanomuse cloud: hands model not written: %s', message(error)))
    this.lastSharedConnectors = ''
    void this.publishConnectors().catch(() => undefined)
    this.signedInCache = true
    this.ctx.logger.info('nanomuse cloud: signed in as %s (%s)', signIn.account.hint, signIn.account.channel)
    await this.profile.pull(signIn.apiKey, true).catch((error: unknown) => this.ctx.logger.warn('nanomuse: profile pull failed: %s', message(error)))
    if (switched) {
      // the last account's device list and notices are not this account's
      this.hub.stop('another account signed in')
      this.notices = []
      this.hub.start()
    } else this.hub.restart()
    if (this.sync) {
      // the account first: the pull that `start` makes runs against the new account's cursor
      await this.sync.accountChanged(signIn.account.id)
      this.sync.start()
    }
    this.broadcast()
    return this.status()
  }

  /** The account's invite code and link; nothing without an account. */
  async invite(): Promise<Invite | undefined> {
    const token = await this.token()
    if (!token || !this.state.account) return undefined
    return this.relay.invite(token)
  }

  /**
   * Rename the agent, or give it an emoji face, for every device of the account:
   * written to the relay, worn here at once (the other devices hear the hub's
   * `profile` frame and pull). A drawn face keeps its pictures when only the
   * name moves.
   */
  writeProfile(patch: { name?: string; avatar?: 'dragon' | 'emoji'; emoji?: string; color?: string }): Promise<Profile> {
    return this.serialize(async () => {
      const token = await this.token()
      const current = this.profile.current()
      const name = (patch.name ?? current.name).trim().slice(0, 60) || current.name || 'nanoMuse'
      const avatar = patch.avatar ?? current.avatar
      const write: ProfileWrite = {
        name,
        avatar,
        emoji: avatar === 'emoji' ? (patch.emoji ?? current.emoji ?? '').slice(0, 16) || '✨' : '',
        color: avatar === 'emoji' ? (patch.color ?? current.color) || '#0064d4' : '',
        style: current.style,
        description: current.description,
      }
      if (token && this.state.account) {
        const rev = await this.relay.putProfile(token, write, this.writerId())
        await this.profile.pull(token, true).catch(() => this.profile.wearLocal({ ...current, ...write, rev }))
      } else {
        await this.profile.wearLocal({ ...current, ...write, rev: current.rev })
      }
      this.broadcast()
      return this.profile.current()
    })
  }

  // ---- the avatar studio: the pictures through the relay, the face onto the account ------

  /** The relay's image model when a self-hosted relay explicitly lists one. */
  private imageModel(): string {
    if (!relayMediaEnabled(this.relay.origin)) return ''
    const models = (this.state.models ?? []).filter((m) => m.kind === 'image')
    const pick = models.find((m) => m.recommended) ?? models[0]
    return pick ? pick.id : ''
  }

  /** A self-hosted relay's image models, for the picker (recommended first). */
  private cloudImageOptions(): ModelOption[] {
    if (!this.cloudModels() || !relayMediaEnabled(this.relay.origin)) return []
    const models = (this.state.models ?? []).filter((m) => m.kind === 'image')
    const pick = this.imageModel()
    return models.map((m) => ({ provider: PROVIDER_ID, providerLabel: 'nanoMuse Cloud', id: m.id, name: m.name || m.id, ...(m.id === pick ? { recommended: true } : {}) })).sort((a, b) => Number(Boolean(b.recommended)) - Number(Boolean(a.recommended)))
  }

  /** The image models the pictures may be drawn with: the account's, then each own row's (0.1.41). */
  imageOptions(): ModelOption[] {
    const out = this.cloudImageOptions()
    for (const [id, row] of this.ownWith('image')) {
      const entry = this.catalogue.find((p) => p.id === row.provider)
      const listed = row.models.filter((m) => m.kind === 'image')
      if (listed.length) for (const m of listed) out.push({ provider: id, providerLabel: row.label, id: m.id, name: m.name })
      else if (entry?.defaults.image) out.push({ provider: id, providerLabel: row.label, id: entry.defaults.image, name: entry.defaults.image })
    }
    return out
  }

  /**
   * Where a picture would be drawn (contract section 3): the person's choice when it still
   * exists; else the chat provider's `defaults.image` when new chats answer through an own row
   * with pictures; else a self-hosted relay that lists an image model; else the first own row
   * with image models. `cloud: true` asks that relay whatever the order says. The public Cloud
   * never returns a media endpoint. Nothing when no source has one.
   */
  async imageEndpoint(opts: { cloud?: boolean; auto?: boolean } = {}): Promise<ImageEndpoint | undefined> {
    const cloud = async (model = this.imageModel()): Promise<ImageEndpoint | undefined> => {
      const token = (opts.cloud ? this.signedInCache && this.state.account : this.cloudModels()) ? await this.token() : undefined
      if (!token || !model) return undefined
      return { shape: 'cloud', baseURL: this.relay.openaiBase, apiKey: token, model, label: 'nanoMuse Cloud', instanceId: PROVIDER_ID }
    }
    if (opts.cloud) return cloud()
    const media = this.state.media ?? {}
    const own = async (id: string, model: string): Promise<ImageEndpoint | undefined> => {
      const row = this.state.providers?.[id]
      if (!row || !model || !row.capabilities.includes('image')) return undefined
      const apiKey = row.keyRef ? await this.credential(row.keyRef) : ''
      if (!apiKey && row.keyRef) return undefined
      return { shape: imageShapeOf(row.provider, row.baseURL), baseURL: row.baseURL, apiKey, model, label: row.label, instanceId: id }
    }
    if (media.imageModel && !opts.auto) {
      if (!media.imageProvider || media.imageProvider === PROVIDER_ID) {
        if ((this.state.models ?? []).some((m) => m.kind === 'image' && m.id === media.imageModel)) {
          const ep = await cloud(media.imageModel)
          if (ep) return ep
        }
      } else if (this.imageOptions().some((o) => o.provider === media.imageProvider && o.id === media.imageModel)) {
        const ep = await own(media.imageProvider, media.imageModel)
        if (ep) return ep
      }
    }
    const chat = this.chatOwnRow()
    if (chat) {
      const ep = await own(chat.id, this.ownModelFor(chat.row, 'image'))
      if (ep) return ep
    }
    const account = await cloud()
    if (account) return account
    for (const [id, row] of this.ownWith('image')) {
      const ep = await own(id, this.ownModelFor(row, 'image'))
      if (ep) return ep
    }
    return undefined
  }

  /** Settings → Models → Making pictures: the image model and where it lives (`nanomuse` or an own row). */
  async setImageModel(id: string, provider = PROVIDER_ID): Promise<void> {
    if (id && !this.imageOptions().some((o) => o.id === id && o.provider === provider)) throw new RelayError(400, 'bad_model', 'Not an image model of the account or of an own key')
    const next: MediaState = { ...this.state.media }
    if (id) {
      next.imageModel = id
      next.imageProvider = provider
    } else {
      delete next.imageModel
      delete next.imageProvider
    }
    this.state = { ...this.state, media: next }
    await this.writeState()
    this.broadcast()
  }

  /** Settings → Models → Making clips: the video model and where it lives; '' puts the order back, `off` keeps the face still. */
  async setVideoModel(id: string, provider = PROVIDER_ID): Promise<void> {
    const next: MediaState = { ...this.state.media }
    if (id === VIDEO_OFF) {
      next.videoModel = VIDEO_OFF
      delete next.videoProvider
    } else if (id) {
      const options = await this.videoOptions()
      if (!options.some((o) => o.id === id && o.provider === provider) && !(provider !== PROVIDER_ID && this.state.providers?.[provider] && (looksLikeVideoModel(id) || KNOWN_DASHSCOPE_MODELS.includes(id)))) {
        throw new RelayError(400, 'bad_model', 'Not a video model of the account or of an own key')
      }
      next.videoModel = id
      next.videoProvider = provider
    } else {
      delete next.videoModel
      delete next.videoProvider
    }
    this.state = { ...this.state, media: next }
    await this.writeState()
    this.broadcast()
  }

  /** The video models the clips may be drawn with: the account's (recommended first), then each own Model Studio row's known ones. */
  async videoOptions(): Promise<ModelOption[]> {
    const out: ModelOption[] = []
    if (this.cloudModels()) {
      const cloud = this.cloudVideoModels()
      const pick = (cloud.find((m) => m.recommended) ?? cloud[0])?.id
      for (const m of cloud) out.push({ provider: PROVIDER_ID, providerLabel: 'nanoMuse Cloud', id: m.id, name: m.name || m.id, ...(m.id === pick ? { recommended: true } : {}) })
      out.sort((a, b) => Number(Boolean(b.recommended)) - Number(Boolean(a.recommended)))
    }
    for (const own of await this.dashScopeProviders()) {
      const known = this.state.media?.checked?.[own.host]
      const ids = known && Date.now() - known.at < VIDEO_CHECK_TTL_MS && known.models.length ? known.models : KNOWN_DASHSCOPE_MODELS
      for (const id of ids) out.push({ provider: own.id, providerLabel: own.label, id, name: id })
    }
    return out
  }

  /**
   * The source the studio would draw with, and whether the account would bill it. Throws
   * `signed_out` only when nothing at all can draw and the person is signed out, so an own
   * image provider draws without an account.
   */
  private async studioEndpoint(viaCloud = false): Promise<ImageEndpoint> {
    const ep = await this.imageEndpoint({ cloud: viaCloud })
    if (ep) return ep
    if (relayMediaEnabled(this.relay.origin) && (!this.signedInCache || !this.state.account)) {
      throw new RelayError(401, 'signed_out', 'Sign in to use this relay')
    }
    throw new RelayError(409, 'no_image_model', 'Nothing configured has an image model to draw with')
  }

  /**
   * What four candidates and four poses would cost today — and the four clips, when the account
   * would draw them (C3). With an own image provider nothing is billed by the account: the
   * estimate says so (`source: provider`) and names the provider and model.
   */
  async studioEstimate(): Promise<Estimate> {
    const ep = await this.studioEndpoint()
    const video = await this.videoEndpoint()
    const clips = video && video.instanceId === PROVIDER_ID && this.state.media?.animate !== false ? ANIMATED.length : 0
    if (ep.instanceId !== PROVIDER_ID) {
      return { cny: 0, leftCny: 0, unlimited: true, affordable: true, imageModel: ep.model, clips, videoModel: clips ? (video?.model ?? '') : '', source: 'provider', label: ep.label }
    }
    const token = await this.token()
    if (!token) throw new RelayError(401, 'signed_out', 'Sign in to draw a face')
    const estimate = await this.relay.estimate(token, STUDIO_PICTURES, clips)
    return { ...estimate, source: 'cloud', label: 'nanoMuse Cloud' }
  }

  // ---- Settings → Media: the video model and the face's clips (desk-b) -------------------

  /**
   * A face arrived (drawn here or on another device): the clips another device made of it are
   * taken from the account first (relay 0.29); what is still missing is drawn when the setting
   * and a video model allow; one line in the log when not.
   */
  private async animateNewFace(): Promise<void> {
    const face = this.faceId()
    const before = this.motion.view().faceId
    const adopted = await this.adoptAccountClips()
    if (adopted) this.ctx.logger.info('nanomuse: avatar motion: %d clip(s) of the face taken from the account', adopted)
    const started = await this.motion.faceChanged()
    if (started) this.ctx.logger.info('nanomuse: avatar motion: drawing the clips of the new face')
    else if (face && face !== before && !adopted && this.state.media?.animate !== false && !(await this.videoEndpoint())) {
      this.ctx.logger.info('nanomuse: avatar motion: no video model (the account lists none and no Model Studio key is set; OpenRouter and the like have no video) — the face keeps still')
    }
  }

  /** The clips the account lists for the worn face, downloaded for the moods this computer has none of; how many came. */
  private async adoptAccountClips(): Promise<number> {
    const face = this.faceId()
    const listed = this.profile.accountClips()
    const token = face && listed ? await this.token() : undefined
    if (!face || !listed || !token) return 0
    const fetchers: Partial<Record<MotionMood, () => Promise<Uint8Array | undefined>>> = {}
    for (const mood of ANIMATED) {
      const clip = listed[mood]
      if (!clip) continue
      fetchers[mood] = async () => {
        const { data, mime } = await this.relay.getBlob(token, clip.sha256)
        if (!mime.startsWith('video/') && !clip.mime.startsWith('video/')) return undefined
        if (createHash('sha256').update(data).digest('hex') !== clip.sha256) return undefined
        return data
      }
    }
    return this.motion.adopt(face, fetchers)
  }

  /**
   * A clip drawn here goes to the account for its other devices (relay 0.29): the file by its
   * hash, then the profile's `clips` with this mood added to what the account already lists.
   * Signed out, or on a relay that keeps no files, nothing is sent; the clip stays here.
   */
  private async shareClip(mood: MotionMood, bytes: Uint8Array): Promise<void> {
    const token = await this.token()
    const face = this.faceId()
    if (!token || !this.state.account || !face || !this.state.account.contribute?.record?.on) return
    const sha256 = createHash('sha256').update(bytes).digest('hex')
    if (!(await this.relay.hasBlob(token, sha256))) await this.relay.putBlob(token, sha256, 'video/mp4', bytes)
    // the face may have moved on while the clip was drawn: then it is not this face's clip any more
    if (this.faceId() !== face) return
    const clips = { ...(this.profile.accountClips() ?? {}), [mood]: { sha256, mime: 'video/mp4', bytes: bytes.length } }
    await this.relay.putProfileClips(token, clips, this.writerId())
    this.profile.rememberClips(face, clips)
  }

  /** The id of the face worn now; empty for the dragon or an emoji. */
  private faceId(): string {
    const p = this.profile.current()
    return p.avatar === 'face' ? p.faceId : ''
  }

  /** A mood's still of the worn face, as the video model's first frame (the account's 512 px WebP). */
  private async faceStill(mood: MotionMood): Promise<{ bytes: Uint8Array; mime: string; name: string } | undefined> {
    const id = this.faceId()
    const path = id ? this.profile.stillPath(id, mood, 'webp') : undefined
    if (!path) return undefined
    try {
      const bytes = await readFile(path)
      return bytes.length ? { bytes: new Uint8Array(bytes), mime: 'image/webp', name: `${mood}.webp` } : undefined
    } catch {
      return undefined
    }
  }

  /** A self-hosted relay's video models. Public Cloud media is retired. */
  private cloudVideoModels(): RelayModel[] {
    if (!relayMediaEnabled(this.relay.origin)) return []
    return (this.state.models ?? []).filter((m) => m.kind === 'video')
  }

  /**
   * Where a clip would be drawn (contract section 3): the person's choice when it still exists
   * (an account model, or an own Model Studio row's); else the chat provider's `defaults.video`
   * when new chats answer through an own row with clips; else the account when signed in and it
   * lists a video model (the relay mirrors Model Studio's `/api/v1` paths); else the first own
   * Model Studio key among the providers the person added. Nothing when the setting says off,
   * or no key speaks DashScope (OpenRouter and the like have no video API). `cloud: true` asks
   * for the account whatever the order says.
   */
  async videoEndpoint(opts: { cloud?: boolean; auto?: boolean } = {}): Promise<VideoEndpoint | undefined> {
    const media = this.state.media ?? {}
    if (media.videoModel === VIDEO_OFF && !opts.cloud && !opts.auto) return undefined
    const token = (opts.cloud ? this.signedInCache && this.state.account : this.cloudModels()) ? await this.token() : undefined
    const cloudModels = this.cloudVideoModels()
    const cloud = (wanted?: string): VideoEndpoint | undefined => {
      if (!token || !cloudModels.length) return undefined
      const model = wanted && cloudModels.some((m) => m.id === wanted) ? wanted : (cloudModels.find((m) => m.recommended) ?? cloudModels[0])?.id
      return model ? { host: this.relay.origin, apiKey: token, model, label: 'nanoMuse Cloud', instanceId: PROVIDER_ID } : undefined
    }
    if (opts.cloud) return cloud(media.videoProvider === PROVIDER_ID ? media.videoModel : undefined)
    const rows = await this.dashScopeProviders()
    const own = (row: (typeof rows)[number], wanted?: string): VideoEndpoint => {
      const known = media.checked?.[row.host]
      const models = known && Date.now() - known.at < VIDEO_CHECK_TTL_MS ? known.models : []
      let model = wanted && (!models.length || models.includes(wanted)) ? wanted : ''
      if (!model) {
        const ours = this.state.providers?.[row.id]
        model = models[0] || (ours ? this.ownModelFor(ours, 'video') : '') || DEFAULT_VIDEO_MODEL
      }
      return { host: row.host, apiKey: row.apiKey, model, label: row.label, instanceId: row.id }
    }
    if (media.videoModel && media.videoModel !== VIDEO_OFF && !opts.auto) {
      if (media.videoProvider === PROVIDER_ID) {
        const ep = cloud(media.videoModel)
        if (ep) return ep
      } else if (media.videoProvider) {
        const row = rows.find((r) => r.id === media.videoProvider)
        if (row) return own(row, media.videoModel)
      } else {
        // 0.1.40 and before: a model without a source, whichever lists it
        const ep = cloud(media.videoModel)
        if (ep && ep.model === media.videoModel) return ep
        if (rows[0] && !cloudModels.some((m) => m.id === media.videoModel)) return own(rows[0], media.videoModel)
        if (ep) return ep
      }
    }
    const chat = this.chatOwnRow()
    if (chat) {
      const row = rows.find((r) => r.id === chat.id)
      if (row) return own(row, this.ownModelFor(chat.row, 'video'))
    }
    const account = cloud()
    if (account) return account
    return rows[0] ? own(rows[0]) : undefined
  }

  /** The provider rows the person added that point at Model Studio, with their keys (the order the settings list them). */
  private async dashScopeProviders(): Promise<Array<{ id: string; host: string; apiKey: string; label: string }>> {
    let rows: Record<string, unknown> = {}
    try {
      const row = this.ctx.settings.describe().find((d) => d.ns === LLM_ROW)
      const value = row?.value as { providers?: Record<string, unknown> } | undefined
      rows = value?.providers ?? {}
    } catch {
      return []
    }
    const out: Array<{ id: string; host: string; apiKey: string; label: string }> = []
    for (const [id, raw] of Object.entries(rows)) {
      if (id === PROVIDER_ID || !raw || typeof raw !== 'object') continue
      const p = raw as { displayName?: unknown; baseURL?: unknown; apiKeyEnv?: unknown; apiKey?: unknown }
      const baseURL = typeof p.baseURL === 'string' ? p.baseURL : ''
      // a Model Studio host, or one of our rows the catalogue gives clips to (the same API behind another address)
      if (!baseURL || !(speaksDashScope(baseURL) || this.state.providers?.[id]?.capabilities.includes('video'))) continue
      let apiKey = ''
      if (typeof p.apiKeyEnv === 'string' && p.apiKeyEnv) {
        try {
          apiKey = (await this.ctx.credentials.resolve(credentialRef(p.apiKeyEnv)))?.value ?? ''
        } catch {
          apiKey = ''
        }
      }
      if (!apiKey && typeof p.apiKey === 'string') apiKey = p.apiKey
      if (!apiKey) continue
      out.push({ id, host: hostOf(baseURL), apiKey, label: typeof p.displayName === 'string' && p.displayName ? p.displayName : id })
    }
    return out
  }

  /** The first provider row the person added that points at Model Studio, with its key. */
  private async dashScopeProvider(): Promise<{ id: string; host: string; apiKey: string; label: string } | undefined> {
    return (await this.dashScopeProviders())[0]
  }

  /** The Media page: models, the switch, the clips. */
  async media(): Promise<MediaView> {
    const media = this.state.media ?? {}
    const off = media.videoModel === VIDEO_OFF
    const signedIn = this.cloudModels()
    const view: MediaView = {
      imageModel: signedIn ? this.imageModel() : '',
      image: { source: 'none', label: '', model: '', reason: 'no_image' },
      video: { source: 'none', label: '', model: '', models: [], off, reason: '' },
      animate: media.animate !== false,
      motion: this.motion.view(),
    }
    // Pictures: the image slot's resolution (Settings → Models); nothing configured has one → `no_image`,
    // and the page says so rather than asking the cloud.
    const image = await this.imageEndpoint()
    if (image) view.image = { source: image.instanceId === PROVIDER_ID ? 'cloud' : 'provider', label: image.label, model: image.model, reason: '' }
    const video = off ? undefined : await this.videoEndpoint()
    const source = video ?? (signedIn ? await this.videoEndpoint({ cloud: true }) : undefined) ?? (await this.dashScopeProvider().then((own) => (own ? { instanceId: own.id, label: own.label, host: own.host } : undefined)))
    if (source) {
      const cloud = source.instanceId === PROVIDER_ID
      const known = cloud ? undefined : media.checked?.[source.host]
      const ids = cloud ? this.cloudVideoModels().map((m) => ({ id: m.id, name: m.name || m.id })) : (known && Date.now() - known.at < VIDEO_CHECK_TTL_MS ? known.models : KNOWN_DASHSCOPE_MODELS).map((id) => ({ id, name: id }))
      view.video = { source: cloud ? 'cloud' : 'provider', label: source.label, model: video?.model ?? '', models: ids, off, reason: cloud || known ? '' : 'unchecked' }
      return view
    }
    // nothing configured has video models (C11): one sentence, not a call to the cloud
    view.video.reason = this.capabilities().includes('video') ? (signedIn ? 'no_cloud_video' : 'no_provider') : 'no_video'
    return view
  }

  /** Settings → Media: the video model (`off` for none) and the animate switch. */
  async setMedia(patch: { videoModel?: string; videoProvider?: string; animate?: boolean }): Promise<MediaView> {
    if (patch.videoModel !== undefined) {
      const id = patch.videoModel.trim()
      let provider = patch.videoProvider
      if (!provider && id && id !== VIDEO_OFF) {
        // the Media page of 0.1.40 sends the id alone: the source that lists it
        provider = this.cloudVideoModels().some((m) => m.id === id) ? PROVIDER_ID : ((await this.dashScopeProvider())?.id ?? PROVIDER_ID)
      }
      await this.setVideoModel(id, provider)
    }
    if (patch.animate !== undefined) {
      const next: MediaState = { ...this.state.media }
      if (patch.animate) delete next.animate
      else next.animate = false
      this.state = { ...this.state, media: next }
      await this.writeState()
      this.broadcast()
    }
    return this.media()
  }

  /** Which of the known Model Studio video models the own key reaches (one empty task each; nothing is billed). */
  async checkVideoModels(): Promise<string[]> {
    const own = await this.dashScopeProvider()
    if (!own) throw new RelayError(409, 'no_provider', 'No Model Studio key among the providers')
    const found: string[] = []
    for (const id of KNOWN_DASHSCOPE_MODELS) {
      if ((await probeVideoModel(own.host, own.apiKey, id)) === true) found.push(id)
    }
    const checked = { ...this.state.media?.checked, [own.host]: { models: found, at: Date.now() } }
    this.state = { ...this.state, media: { ...this.state.media, checked } }
    await this.writeState()
    this.broadcast()
    return found
  }

  /** Settings → Computer use: a test screenshot or a small mouse move through a fresh `nanomuse mcp`. */
  async handsCheck(kind: 'screenshot' | 'move'): Promise<ScreenshotCheck | MoveCheck> {
    const runtime = await runtimeInfo()
    if (!runtime.ok) {
      const error = runtime.problem === 'not-found' ? 'No nanomuse runtime: NANOMUSE_PY is not set and `nanomuse` is not on PATH' : `NANOMUSE_PY points at ${runtime.path}, which ${runtime.problem === 'missing' ? 'does not exist' : 'is not executable'}`
      return kind === 'screenshot' ? { ok: false, black: false, error } : { ok: false, accessibility: false, error }
    }
    const options = { command: runtime.path, args: ['mcp'], env: { NANOMUSE_MCP_CONFIRM: process.env.NANOMUSE_MCP_CONFIRM ?? '' }, timeoutMs: 45_000 }
    const result = kind === 'screenshot' ? await checkScreenshot(options) : await checkMove(options)
    if (kind === 'screenshot') {
      const shot = result as ScreenshotCheck
      if (shot.black) this.sawBlackScreen()
      else if (shot.ok && this.blackScreenAt) {
        this.blackScreenAt = 0
        this.broadcast()
      }
    }
    return result
  }

  /** Which binary the hands run, how its last mount went, and on Linux the display session they would work on (Settings → Computer use). */
  async runtime(): Promise<RuntimeInfo & { mount: HandsMount }> {
    const info = await runtimeInfo()
    const display = displayInfo()
    return { ...info, ...(display ? { display } : {}), mount: this.handsStatus() }
  }

  private sawBlackScreen(): void {
    this.blackScreenAt = Date.now()
    this.broadcast()
  }

  /**
   * One candidate, drawn from the words; PNG bytes as the model gave them. The image slot's
   * source draws it: the account through the relay, an own row through its own API
   * (`images.ts`). `viaCloud` is *Use nanoMuse Cloud this time*: the account for this one
   * picture, the slot untouched.
   */
  async studioDraw(prompt: string, viaCloud = false): Promise<Buffer> {
    const ep = await this.studioEndpoint(viaCloud)
    if (ep.instanceId === PROVIDER_ID) return this.relay.generateImage(ep.apiKey, ep.model, prompt)
    return ownGenerateImage(ep, prompt)
  }

  /** One pose of the chosen candidate, by the same source as the candidate. */
  async studioPose(image: Buffer, prompt: string, viaCloud = false): Promise<Buffer> {
    const ep = await this.studioEndpoint(viaCloud)
    if (ep.instanceId === PROVIDER_ID) return this.relay.editImage(ep.apiKey, ep.model, image, prompt)
    return ownEditImage(ep, image, prompt)
  }

  /**
   * Wear a face drawn here: the stills go to the account (`PUT /v1/me/profile` with the
   * `face` map), then the profile is pulled so the pictures land under `faces/<id>/`
   * and every device of the account hears the hub's `profile` frame. Signed out, the face
   * is worn on this computer alone (`ProfileStore.wearDrawn`): the pictures were drawn by
   * your own provider and are already here, so a sign-in gates only the other devices.
   */
  wearFace(description: string, style: string, face: Record<string, string>): Promise<Profile> {
    return this.serialize(async () => {
      const token = await this.token()
      if (!token || !this.state.account) {
        const faceId = await this.profile.wearDrawn(face, description.slice(0, 200), style.slice(0, 20))
        if (!faceId) throw new RelayError(400, 'bad_face', 'The drawn face has no idle still')
        this.broadcast()
        return this.profile.current()
      }
      const current = this.profile.current()
      const write: ProfileWrite = {
        name: current.name || 'nanoMuse',
        avatar: 'face',
        emoji: '',
        color: current.color,
        style: style.slice(0, 20),
        description: description.slice(0, 200),
        face,
        // the clips on the account were of the old face
        clips: null,
      }
      await this.relay.putProfile(token, write, this.writerId())
      await this.profile.pull(token, true)
      this.broadcast()
      return this.profile.current()
    })
  }

  /** Pull the account and the model list again; a retired key signs out. */
  refresh(): Promise<CloudStatus> {
    return this.serialize(async () => {
      const token = await this.token()
      if (!token) return this.status()
      try {
        const [account, models] = await Promise.all([this.relay.me(token), this.relay.models(token)])
        // The row is rewritten when the menu changed — and when it is simply not there: a profile
        // made again around an account that is still signed in has the credential but no row.
        if (!sameModels(models, this.state.models ?? []) || !this.providerPresent() || !this.providerBudgetCurrent()) await this.writeProvider(models)
        await this.adoptDefaultModel(models)
        this.state = { ...this.state, account, models }
        this.recordOff = false
        await this.writeState()
        await this.writeHands().catch((error: unknown) => this.ctx.logger.warn('nanomuse cloud: hands model not written: %s', message(error)))
        await this.profile.pull(token).catch((error: unknown) => this.ctx.logger.warn('nanomuse: profile pull failed: %s', message(error)))
        if (!this.hub.connected) this.hub.start()
      } catch (error: unknown) {
        if (error instanceof RelayError && error.status === 401) {
          await this.forget()
          return { ...(await this.status()), error: { code: error.code, message: error.message } }
        }
        throw error
      }
      this.broadcast()
      return this.status()
    })
  }

  /** Data controls: flip the switch on the relay and keep the account row current. */
  setContribute(on: boolean): Promise<CloudStatus> {
    return this.serialize(async () => {
      const token = await this.token()
      if (!token || !this.state.account) return this.status()
      const contribute = await this.relay.setContribute(token, on)
      // a relay before 0.30 answers the flip without the record's figures: the ones read with the account stay
      const record = contribute.record ?? this.state.account.contribute?.record
      this.state = { ...this.state, account: { ...this.state.account, contribute: { ...contribute, ...(record ? { record } : {}) } } }
      await this.writeState()
      return this.status()
    })
  }

  /** Data controls: delete what the relay kept; the count goes to zero. */
  deleteSamples(): Promise<CloudStatus & { deleted: number }> {
    return this.serialize(async () => {
      const token = await this.token()
      if (!token || !this.state.account) return { ...(await this.status()), deleted: 0 }
      const deleted = await this.relay.deleteSamples(token)
      const previous = this.state.account.contribute
      if (previous) {
        // the record went with the training turns: its figures read zero until the account is read again
        const record = previous.record ? { ...previous.record, turns: 0, own: 0, files: 0, mediaBytes: 0 } : undefined
        this.state = { ...this.state, account: { ...this.state.account, contribute: { ...previous, samples: 0, ...(record ? { record } : {}) } } }
        await this.writeState()
      }
      return { ...(await this.status()), deleted }
    })
  }

  /** Retire this device's key and take the provider out of the picker. */
  signOut(): Promise<CloudStatus> {
    return this.serialize(async () => {
      const token = await this.token()
      if (token) {
        try {
          await this.relay.signOut(token)
        } catch (error: unknown) {
          this.ctx.logger.warn('nanomuse cloud: the relay did not take the sign-out: %s', message(error))
        }
      }
      await this.forget()
      return this.status()
    })
  }

  /** Fetch the account's look now (a `profile` frame, or the person asked). */
  async pullProfile(force = false): Promise<Profile> {
    const token = await this.token()
    if (token) await this.profile.pull(token, force)
    return this.profile.current()
  }

  /** Rename this computer on the account's list (kept locally too, for the next hello). */
  async renameDevice(name: string): Promise<void> {
    const clean = name.trim().slice(0, 60)
    if (!clean) return
    this.state = { ...this.state, deviceName: clean }
    await this.writeState()
    this.hub.rename(clean)
    this.broadcast()
  }

  /** Whether every device of the account may run things here without asking; off (the default) means each one asks. */
  get remoteControl(): boolean {
    return this.state.remoteControl === true
  }

  /** Flip remote control. */
  async setRemoteControl(on: boolean): Promise<void> {
    if (this.remoteControl === on) return
    this.state = { ...this.state, remoteControl: on }
    await this.writeState()
    this.ctx.logger.info('nanomuse: remote control %s', on ? 'on (no questions)' : 'off (each device asks)')
    this.broadcast()
  }

  /** The devices allowed without asking. */
  get trusted(): TrustedDevice[] {
    return Object.entries(this.state.trusted ?? {}).map(([id, v]) => ({ id, name: v.name, at: v.at })).sort((a, b) => a.at - b.at)
  }

  /** Allow, or stop allowing, one device without asking. */
  async setTrusted(id: string, name: string, on: boolean): Promise<void> {
    const trusted = { ...this.state.trusted }
    if (on) trusted[id] = { name: name || trusted[id]?.name || id, at: Date.now() }
    else if (id in trusted) delete trusted[id]
    else return
    this.state = { ...this.state, trusted }
    await this.writeState()
    this.ctx.logger.info('nanomuse: %s %s without asking', name || id, on ? 'allowed' : 'no longer allowed')
    this.broadcast()
  }

  /**
   * Whether `from` may do `action` here now. Remote control on, or a device allowed without asking: yes.
   * Otherwise a card on this screen asks the person; *always* remembers the device. No answer in time, or
   * a no, and the asker hears `not_allowed`.
   */
  private async permit(from: Caller, action: string, text: string): Promise<void> {
    if (this.remoteControl || this.state.trusted?.[from.id]) return
    this.ctx.logger.info('nanomuse: %s asks to %s here%s', from.name || 'a device', action, text ? `: ${text}` : '')
    const answer = await this.asks.ask(from, action, text)
    if (answer === 'always') await this.setTrusted(from.id, from.name, true)
    if (answer === 'once' || answer === 'always') return
    throw new HubError(
      'not_allowed',
      answer === 'timeout' ? `nobody at ${this.deviceName()} answered in time` : `the person at ${this.deviceName()} did not allow it`,
    )
  }

  /** The current account key, for a plugin that speaks to the relay itself. */
  token(): Promise<string | undefined> {
    return this.ctx.credentials.resolve(credentialRef(TOKEN_REF)).then((r) => r?.value)
  }

  // -- the provider row -----------------------------------------------------------

  /**
   * The `nanomuse` provider as the Models page would have written it — with the image budget
   * of a request that goes through the relay: the relay refuses a body past its cap (413
   * `too_large`), and a computer-use turn accumulates screenshots, so the base64 images of one
   * request are held to `IMAGE_BUDGET` (the oldest offloaded, as `dsh-llm-pi-ai` does past the
   * budget) and each is scaled to the pixel budget before it is encoded.
   */
  providerRow(models: RelayModel[]): Record<string, unknown> {
    return providerRowFor(this.relay.openaiBase, models, this.state.deviceId ?? '')
  }

  private async writeProvider(models: RelayModel[]): Promise<void> {
    await this.ctx.settings.update(LLM_ROW, { providers: { [PROVIDER_ID]: this.providerRow(models) } })
  }

  /**
   * New sessions answer through the account when nothing else would: dsh's stock default is
   * DeepSeek's own provider, which has no key on a computer that signed in here instead. A
   * choice the person made (any other provider, or a DeepSeek key) is left alone.
   */
  private async adoptDefaultModel(models: RelayModel[]): Promise<void> {
    const pick = pickChatModel(models)
    if (!pick) return
    const svc = (this.ctx as unknown as { get(name: string): unknown }).get('agentDefaultModel') as
      | { currentSelection(): { provider: string; model: string }; saveSelection(next: { provider: string; model: string }): Promise<void> }
      | undefined
    if (!svc) return
    try {
      const current = svc.currentSelection()
      if (current.provider === PROVIDER_ID) return
      if (this.state.cloudModelsOff) return // switched off under Settings › Models: a sign-in changes no slot
      if (current.provider !== 'deepseek-official' && current.provider !== 'deepseek-account') return
      if (await this.otherProviderReady()) return
      await svc.saveSelection({ provider: PROVIDER_ID, model: pick.id })
      this.ctx.logger.info('nanomuse cloud: new sessions answer through %s/%s', PROVIDER_ID, pick.id)
    } catch (error: unknown) {
      this.ctx.logger.warn('nanomuse cloud: could not make the account model the default: %s', message(error))
    }
  }

  private async forget(): Promise<void> {
    this.hub.stop('signed out')
    await this.ctx.credentials.unset(credentialRef(TOKEN_REF))
    try {
      await this.ctx.settings.mutate(LLM_ROW, [{ op: 'unset', path: ['providers', PROVIDER_ID] }])
    } catch (error: unknown) {
      // The row may never have had the provider (a sign-in that failed half-way).
      this.ctx.logger.debug('nanomuse cloud: provider row not removed: %s', message(error))
    }
    // the sync state stays (C10): it remembers which account the mapped sessions belong to, so a
    // different account signing in next is known and the last account's chats are hidden, not pushed
    // the own keys and the ChatGPT sign-in are this computer's, not the account's (C11): they stay too
    const { deviceId, deviceName, remoteControl, sync, providers, chatgpt, media, handsProvider, handsModel, mordeHub } = this.state
    this.state = {
      ...(deviceId ? { deviceId } : {}),
      ...(deviceName ? { deviceName } : {}),
      ...(remoteControl === true ? { remoteControl } : {}),
      ...(sync ? { sync } : {}),
      ...(providers ? { providers } : {}),
      ...(chatgpt ? { chatgpt } : {}),
      ...(media ? { media } : {}),
      ...(handsProvider && handsModel ? { handsProvider, handsModel } : {}),
      ...(mordeHub ? { mordeHub } : {}),
    }
    this.sync?.stop()
    this.sync?.signedOut()
    this.signedInCache = false
    this.lastSharedConnectors = ''
    // the account's route is gone from the model layer: a chat slot left on it would fail every turn with NO_ADAPTER
    await this.releaseChatRoute(PROVIDER_ID)
    await this.writeState()
    await this.writeHands().catch(() => rm(join(this.dir(), 'hands.json'), { force: true }).catch(() => undefined))
    await this.profile.reset()
    this.broadcast()
  }

  /**
   * The model ids the model adapter's catalog route `id` serves, as its catalog lists them (no
   * network call: the adapter answers a route it describes from its own registry). Empty when
   * the harness cannot say; the catalogue's default model is then the row's one model.
   */
  private async catalogRouteModels(id: string): Promise<string[]> {
    const llm = (this.ctx as unknown as { get(name: string): unknown }).get('llm') as { discoverModels?: (ns: string, request: { provider: string }) => Promise<{ id: string }[]> } | undefined
    try {
      const found = (await llm?.discoverModels?.(LLM_ROW, { provider: id })) ?? []
      return found.map((m) => m.id).filter((m) => typeof m === 'string' && m.length > 0)
    } catch (error) {
      this.ctx.logger.warn('nanomuse: the models of the %s route are not known: %s', id, message(error))
      return []
    }
  }

  /** The providers the model layer knows right now. */
  private providers(): { id: string }[] {
    const llm = (this.ctx as unknown as { get(name: string): unknown }).get('llm') as { listProviders?: () => { id: string }[] } | undefined
    return llm?.listProviders?.() ?? []
  }

  /** Whether the `nanomuse` provider row is in the model layer (it lives in the profile's patch file). */
  private providerPresent(): boolean {
    return this.providers().some((p) => p.id === PROVIDER_ID)
  }

  /** Whether the row written earlier carries today's image budget: a budget change counts as a changed menu. */
  private providerBudgetCurrent(): boolean {
    try {
      const row = this.ctx.settings.describe().find((d) => d.ns === LLM_ROW)
      const providers = (row?.value as { providers?: Record<string, unknown> } | undefined)?.providers ?? {}
      const ours = providers[PROVIDER_ID]
      if (!ours || typeof ours !== 'object') return false
      return Object.entries(IMAGE_BUDGET).every(([key, value]) => (ours as Record<string, unknown>)[key] === value)
    } catch {
      return true
    }
  }

  /** Another model can answer without the account: a DeepSeek key, or a provider the person added. */
  private async otherProviderReady(): Promise<boolean> {
    try {
      if ((await this.ctx.credentials.resolve(credentialRef('DEEPSEEK_API_KEY')))?.value) return true
    } catch {
      // no credential store answer: assume nothing
    }
    return this.providers().some((p) => p.id !== 'deepseek-official' && p.id !== 'deepseek-account' && p.id !== PROVIDER_ID)
  }

  // -- hands and notices ----------------------------------------------------------------

  private began(callId: string, name: string, args: unknown, sessionId: string): void {
    const now = Date.now()
    if (now - this.lastCallAt > 60_000 && this.calls.size === 0) this.steps = 0
    this.lastCallAt = now
    this.steps += 1
    this.calls.set(callId, { id: callId, name, args: pickArgs(args), sessionId, since: now })
    this.trajectory.began(sessionId, callId)
    this.screenRuns.began(sessionId, name, now)
    this.armRunSweep()
    this.broadcast()
  }

  private ended(callId: string): void {
    const call = this.calls.get(callId)
    if (call && this.calls.delete(callId)) {
      this.lastCallAt = Date.now()
      this.screenRuns.ended(call.sessionId, this.lastCallAt)
      this.armRunSweep()
      this.broadcast()
    }
    // the stage empties itself a while after the hands rest
    if (this.stageTimer) clearTimeout(this.stageTimer)
    this.stageTimer = setTimeout(() => {
      this.stageTimer = undefined
      if (this.calls.size === 0 && Date.now() - this.lastCallAt >= STAGE_REST_MS - 1000) this.clearStage()
    }, STAGE_REST_MS)
    this.stageTimer.unref?.()
  }

  /**
   * The screen runs close with their turn (`turn/end`, below); this is the safety net for a
   * turn whose end never came: a run quiet for `SCREEN_RUN_IDLE_MS` with nothing in flight
   * lets go of the screen, and the glow goes down.
   */
  private armRunSweep(): void {
    if (this.runTimer) clearTimeout(this.runTimer)
    this.runTimer = undefined
    if (this.screenRuns.size === 0) return
    this.runTimer = setTimeout(() => {
      this.runTimer = undefined
      const inFlight = (sessionId: string) => [...this.calls.values()].some((c) => c.sessionId === sessionId)
      if (this.screenRuns.sweep(Date.now(), inFlight)) this.broadcast()
      this.armRunSweep()
    }, SCREEN_RUN_IDLE_MS + 50)
    this.runTimer.unref?.()
  }

  // -- the Live stage -------------------------------------------------------------------

  /** A frame of a screen the agent is working on; `meta.device` names another device, else it is this computer's. */
  stageFrame(bytes: Buffer, mime: string, meta: { device?: string; width?: number; height?: number; title?: string; mode?: 'screen' | 'window'; sessionId: string; callId?: string }): void {
    if (bytes.length === 0 || bytes.length > 12 * 1024 * 1024) return
    const seq = this.trajectory.frame(bytes, mime, meta)
    const sameScreen = this.stage.source === (meta.device ? 'device' : 'computer') && this.stage.device === (meta.device ?? '')
    this.stage = {
      seq,
      at: Date.now(),
      source: meta.device ? 'device' : 'computer',
      device: meta.device ?? '',
      width: meta.width ?? 0,
      height: meta.height ?? 0,
      title: (meta.title ?? '').slice(0, 120),
      mode: meta.mode ?? 'screen',
      // an action aimed at another screen does not belong on this frame
      action: sameScreen ? this.stage.action : null,
      sessionId: meta.sessionId,
    }
    this.broadcast()
  }

  /** What the hands are about to do on this computer's screen: the stage's marker, and a step of the trajectory. */
  private acted(action: StageAction, sessionId: string, callId: string): void {
    this.stage = { ...this.stage, action, sessionId: sessionId || this.stage.sessionId }
    this.trajectory.acted(sessionId, callId, action)
    this.broadcast()
  }

  /**
   * A step the runtime's hands refused without the person's word (Enter, a submit, a click
   * on "pay", …): ask them on the permission card, and if they agree, run the same call
   * again carrying the confirmation ticket the `nanomuse mcp` server checks — an HMAC of
   * the arguments under the secret the desktop shell gave both of us (NANOMUSE_MCP_CONFIRM).
   * Without the secret (a hand-made dsh profile) the server takes `confirmed: true`, so
   * that is what the re-dispatch carries; either way the model never confirms on its own.
   *
   * The second run goes through the *same* execution (`exec` with the ticket added to its
   * arguments, then `next()` once more) rather than a nested `tools.execute`: the registry
   * re-renders a wrapper-authored result from its value through the tool's text projection,
   * and the MCP bridge keeps the admitted screenshot of a result keyed by the execution it
   * ran for — a nested call's picture never reached the model (0.1.37: "Image didn't come
   * through" after every approved step). The arguments the model wrote are put back after.
   */
  private async confirmStep(ctx: Context, exec: ToolDispatchExecution, refused: string, next: () => Promise<ToolExecutionResult>): Promise<ToolExecutionResult> {
    const approval = ctx.get('approval')
    if (!approval || !exec.agent) return declined(refused, 'approval is not available here')
    const step = refused.replace(/\.\s+(The person has to agree|Ask the person)[\s\S]*$/, '').trim()
    const outcome = await approval.request({
      agent: exec.agent,
      toolName: exec.name,
      callId: exec.callId,
      reason: step,
      displayReason: { en: `On this computer's screen: ${step}`, zh: `在这台电脑的屏幕上：${step}` },
      signal: exec.signal,
    })
    if (outcome !== 'allowed-once') return declined(step, outcome)
    const args = exec.arguments && typeof exec.arguments === 'object' && !Array.isArray(exec.arguments) ? (exec.arguments as Record<string, unknown>) : {}
    const secret = process.env.NANOMUSE_MCP_CONFIRM?.trim()
    const confirmed: string | true = secret ? confirmTicket(secret, args) : true
    const mutable = exec as { arguments: unknown }
    const written = mutable.arguments
    mutable.arguments = Object.freeze({ ...args, confirmed })
    try {
      return await next()
    } finally {
      mutable.arguments = written
    }
  }

  /** The screenshot and the words that came back from `computer_screen` / `computer_act` (an MCP result). */
  private frameFromMcp(value: unknown, sessionId: string, callId: string): void {
    const content = (value as { content?: unknown[] } | undefined)?.content
    if (!Array.isArray(content)) return
    let image: { data: string; mime: string } | undefined
    let text = ''
    for (const block of content) {
      if (!block || typeof block !== 'object') continue
      const b = block as { type?: unknown; data?: unknown; mimeType?: unknown; text?: unknown }
      if (b.type === 'image' && typeof b.data === 'string' && !image) image = { data: b.data, mime: typeof b.mimeType === 'string' ? b.mimeType : 'image/png' }
      else if (b.type === 'text' && typeof b.text === 'string') text += (text ? '\n' : '') + b.text
    }
    if (!image) return
    const head = screenHead(text)
    this.stageFrame(Buffer.from(image.data, 'base64'), image.mime, { ...head, sessionId, callId })
  }

  /** The agent has stopped using that screen for a while: the stage can go (the trajectory stays, for looking back). */
  private clearStage(): void {
    if (this.stage.seq === 0) return
    this.stage = NO_STAGE
    this.broadcast()
  }

  private notice(kind: Notice['kind'], from: string, title: string, text: string, action?: string): void {
    this.noticeSeq += 1
    this.notices = [
      ...this.notices.slice(-7),
      { id: this.noticeSeq, kind, from, title: title.slice(0, 80), text: text.slice(0, 500), ...(action ? { action } : {}), at: Date.now() },
    ]
    if (kind === 'notify') this.ctx.logger.info('nanomuse: notice from %s: %s', from, text.slice(0, 80))
    this.broadcast()
  }

  /**
   * The actions this computer announces: `info` and `notify`, the remote ones (the person here
   * agrees to each unless remote control is on), a task once sessions are up, and the coding
   * agents — always, as the runtime does: a computer without any of the CLIs answers with empty
   * lists, and installing one later needs no new hello.
   */
  private actions(): string[] {
    const out = [...ACTIONS, ...REMOTE_ACTIONS]
    if (this.tasks) out.push('approve', 'task', 'stop')
    out.push(...CODING_ACTIONS)
    return out
  }

  /** Another device running something here (`docs/hub.md`: the asker judged it; here the person — or the switch — decides). */
  private async remote(action: RemoteAction, args: Record<string, unknown>, from: Caller): Promise<Record<string, unknown>> {
    const summary = brief(action, args)
    await this.permit(from, action, summary)
    const who = from.name || 'a device'
    this.ctx.logger.info('nanomuse: %s asked %s here%s', who, action, summary ? `: ${summary}` : '')
    const body = await runAction(action, args)
    // Only what actually happened is worth a toast; a refused path is the asker's error to see.
    if (TOAST_ACTIONS.has(action)) this.notice('call', who, '', summary, action)
    return body
  }

  // -- state ----------------------------------------------------------------------------

  private hubState(): HubState {
    return {
      connected: this.hub.connected,
      deviceId: this.state.deviceId ?? '',
      deviceName: this.deviceName(),
      remoteControl: this.remoteControl,
      trusted: this.trusted,
      asks: this.asks.list,
      ...(this.hub.lastError ? { lastError: this.hub.lastError } : {}),
      devices: this.hub.devices,
    }
  }

  private deviceName(): string {
    return this.state.deviceName || this.config.deviceName || hostname() || 'desktop'
  }

  private dir(): string {
    return this.config.statePath ? dirname(this.config.statePath) : join(dshHome(), 'nanomuse')
  }

  private statePath(): string {
    return this.config.statePath || join(this.dir(), 'cloud.json')
  }

  private async readState(): Promise<State> {
    try {
      const raw = JSON.parse(await readFile(this.statePath(), 'utf8')) as State
      const mordeHub = mordeHubStateOf(raw.mordeHub)
      return {
        ...(raw.account ? { account: raw.account } : {}),
        ...(raw.models ? { models: raw.models } : {}),
        ...(typeof raw.deviceId === 'string' && raw.deviceId ? { deviceId: raw.deviceId } : {}),
        ...(typeof raw.deviceName === 'string' && raw.deviceName ? { deviceName: raw.deviceName } : {}),
        ...(typeof raw.handsModel === 'string' && raw.handsModel ? { handsModel: raw.handsModel } : {}),
        // a grant 0.1.37 kept for the "app" "Done. Screen now:" (the mis-read head line) names nothing — dropped
        ...(Array.isArray(raw.grants) ? { grants: (raw.grants as Grant[]).filter((g) => g && typeof g.id === 'string' && typeof g.target === 'string' && !MISREAD_GRANT.test(g.target) && !isOwnApp(g.target.slice('computer_app:'.length))).map((g) => ({ id: g.id, target: g.target, at: Number(g.at) || 0, ...(typeof g.sessionId === 'string' && g.sessionId ? { sessionId: g.sessionId, sessionTitle: String(g.sessionTitle ?? '') } : {}), ...(typeof g.sessionId === 'string' && g.sessionId && typeof g.scheduleId === 'string' && g.scheduleId ? { scheduleId: g.scheduleId, scheduleTitle: String(g.scheduleTitle ?? '') } : {}) })) } : {}),
        ...(raw.update && typeof raw.update === 'object' && typeof (raw.update as UpdateInfo).checkedAt === 'number' ? { update: updateForThisVersion(raw.update as UpdateInfo) } : {}),
        ...(raw.remoteControl === true ? { remoteControl: true } : {}),
        ...(raw.trusted && typeof raw.trusted === 'object' ? { trusted: trustedOf(raw.trusted) } : {}),
        ...(raw.taskSessions && typeof raw.taskSessions === 'object' ? { taskSessions: raw.taskSessions } : {}),
        ...(raw.sync && typeof raw.sync === 'object' ? { sync: raw.sync } : {}),
        ...(raw.media && typeof raw.media === 'object' ? { media: raw.media } : {}),
        ...(typeof raw.handsProvider === 'string' && raw.handsProvider ? { handsProvider: raw.handsProvider } : {}),
        ...(raw.providers && typeof raw.providers === 'object' ? { providers: ownProvidersOf(raw.providers) } : {}),
        ...(raw.ownProviderKeyCleanup && typeof raw.ownProviderKeyCleanup === 'object' ? { ownProviderKeyCleanup: raw.ownProviderKeyCleanup } : {}),
        ...(raw.chatgpt && typeof raw.chatgpt === 'object' && typeof (raw.chatgpt as ChatGptState).label === 'string' ? { chatgpt: { label: (raw.chatgpt as ChatGptState).label, at: Number((raw.chatgpt as ChatGptState).at) || 0 } } : {}),
        ...(raw.cloudModelsOff === true ? { cloudModelsOff: true } : {}),
        ...(mordeHub ? { mordeHub } : {}),
      }
    } catch {
      return {}
    }
  }

  /** A session's cached log facts, dropped: its log changed. */
  private logChanged(sessionId: string): void {
    this.logCache.delete(sessionId)
  }

  private cached(sessionId: string): { title?: { at: number; title: string }; lines?: SessionLine[]; prompts?: Array<{ key: string; time: number }> } {
    let entry = this.logCache.get(sessionId)
    if (!entry) {
      entry = {}
      this.logCache.set(sessionId, entry)
    }
    return entry
  }

  /** The session's title as the chats column shows it: the latest `session/title` event, else its first prompt. */
  private async sessionTitle(ctx: Context, sessionId: SessionId, updatedAt: number): Promise<string> {
    const entry = this.cached(String(sessionId))
    if (entry.title && entry.title.at === updatedAt) return entry.title.title
    try {
      const inspection = await ctx.sessionController.inspect(sessionId)
      let title = ''
      let first = ''
      for (const event of inspection.events as ReadonlyArray<{ type: string; data: unknown }>) {
        if (event.type === 'session/title') title = String((event.data as { title?: string }).title ?? '')
        else if (!first && event.type === 'user/message' && (event.data as { source?: { kind?: string } }).source?.kind === 'user') first = textOf((event.data as { content?: readonly ContentBlock[] }).content)
      }
      const out = (title || first).replace(/\s+/g, ' ').trim().slice(0, 120)
      entry.title = { at: updatedAt, title: out }
      return out
    } catch {
      return ''
    }
  }

  /** The person's prompts and the model's final texts of one session, for the sync engine. */
  private async sessionLines(ctx: Context, sessionId: string): Promise<SessionLine[]> {
    const entry = this.cached(sessionId)
    if (entry.lines) return entry.lines
    try {
      const inspection = await ctx.sessionController.inspect(sessionId as SessionId)
      const lines: SessionLine[] = []
      // the final assistant text of a turn is the last `assistant/message` before its `turn/end`
      let lastAssistant: SessionLine | undefined
      for (const event of inspection.events) {
        if (event.type === 'user/message') {
          const data = event.data as unknown as { id?: string; content?: readonly ContentBlock[]; source?: { kind?: string } }
          if (data.source?.kind !== 'user') continue
          const text = textOf(data.content)
          if (text && !text.startsWith('[Asked from ')) lines.push({ id: String(data.id ?? `u${event.seq}`), role: 'user', text, at: event.time })
        } else if (event.type === 'assistant/message') {
          const data = event.data as unknown as { message?: { id?: string; content?: readonly ContentBlock[] }; interrupted?: true }
          const text = textOf(data.message?.content)
          if (text && data.interrupted !== true) lastAssistant = { id: String(data.message?.id ?? `a${event.seq}`), role: 'assistant', text, at: event.time }
        } else if (event.type === 'turn/end') {
          if (lastAssistant) lines.push(lastAssistant)
          lastAssistant = undefined
        }
      }
      entry.lines = lines
      return lines
    } catch {
      return []
    }
  }

  /**
   * The turns from the account's other devices kept for one session (C8), for the browser
   * half's bubbles: in time order, each with the key of the first prompt typed here that is
   * younger than it (`before`, the row's `data-chat-node-key`), so the bubble is shown
   * before that prompt; none when the turn is the newest thing in the chat.
   */
  private async remoteLines(
    ctx: Context,
    sessionId: string,
  ): Promise<{ lines: Array<RemoteLine & { id: string; before: string | null }>; prompts: string[] }> {
    const lines = this.kept?.linesOf(sessionId) ?? []
    if (lines.length === 0) return { lines: [], prompts: [] }
    const entry = this.cached(sessionId)
    let prompts = entry.prompts
    if (!prompts) {
      prompts = []
      const sc = ctx.get('sessionController')
      if (sc) {
        try {
          const inspection = await sc.inspect(sessionId as SessionId)
          for (const event of inspection.events) {
            if (event.type !== 'user/message') continue
            const data = event.data as unknown as { id?: string; source?: { kind?: string } }
            const rawTime = Number((event as { time?: number; timestamp?: number }).time ?? (event as { timestamp?: number }).timestamp ?? 0)
            const time = rawTime > 0 && rawTime < 1e11 ? rawTime * 1000 : rawTime
            if (data.source?.kind === 'user') prompts.push({ key: `13:input-message${String(data.id ?? '')}`, time })
          }
          entry.prompts = prompts
        } catch {
          // a session that cannot be read: the bubbles go after whatever is shown
        }
      }
    }
    return {
      lines: lines.map((line) => {
        const seenAt = line.keptAt !== undefined && line.keptAt > 0 ? Math.min(line.at, line.keptAt) : line.at
        const anchor = prompts.find((p) => p.time > seenAt || (line.keptAt !== undefined && line.keptAt > 0 && p.time >= line.keptAt))
        return { ...line, id: line.mid, before: anchor?.key ?? null }
      }),
      prompts: prompts.map((p) => p.key),
    }
  }

  private async writeState(): Promise<void> {
    const path = this.statePath()
    await mkdir(dirname(path), { recursive: true })
    await writeFile(path, JSON.stringify(this.state, null, 2) + '\n', { mode: 0o600 })
  }

  private serialize<T>(work: () => Promise<T>): Promise<T> {
    const next = this.busy.then(work, work)
    this.busy = next.catch(() => undefined)
    return next
  }

  // -- the loopback API -------------------------------------------------------------

  /** Called whenever the live state changes (sign-in, profile, hub, devices); for other services. */
  onChange(listener: () => void): () => void {
    this.changeListeners.add(listener)
    return () => {
      this.changeListeners.delete(listener)
    }
  }

  /**
   * A card went up or came down (an approval the stage may answer, a hold of the hands):
   * the stage re-reads, and the account's other devices hear that the run is waiting for
   * the person, or working again (0.31).
   */
  private deskMoved(): void {
    this.broadcast()
    const sync = this.sync
    if (!sync) return
    const now = new Map<string, string>()
    for (const row of this.approvalDesk.list()) if (!now.has(row.sessionId)) now.set(row.sessionId, row.summary)
    for (const hold of this.holdDesk.list()) if (!now.has(hold.thread)) now.set(hold.thread, hold.reason)
    for (const [sid, words] of now) sync.step(sid, 'waiting', words)
    for (const sid of this.waitingSessions) if (!now.has(sid)) sync.step(sid, 'working', '')
    this.waitingSessions = new Set(now.keys())
  }

  private broadcast(): void {
    for (const listener of this.changeListeners) {
      try {
        listener()
      } catch {
        // a listener's problem is not ours
      }
    }
    if (this.streams.size === 0) return
    const data = `data: ${JSON.stringify(this.live())}\n\n`
    for (const res of this.streams) {
      try {
        res.write(data)
      } catch {
        this.streams.delete(res)
      }
    }
  }

  private stream(req: IncomingMessage, res: ServerResponse): void {
    res.writeHead(200, { 'content-type': 'text/event-stream', 'cache-control': 'no-store', connection: 'keep-alive', 'x-accel-buffering': 'no' })
    res.write(`retry: 2000\ndata: ${JSON.stringify(this.live())}\n\n`)
    this.streams.add(res)
    const beat = setInterval(() => {
      try {
        res.write(': beat\n\n')
      } catch {
        // closed below
      }
    }, 20_000)
    const done = () => {
      clearInterval(beat)
      this.streams.delete(res)
    }
    req.on('close', done)
    res.on('close', done)
  }

  private readonly handle = async (req: IncomingMessage, res: ServerResponse): Promise<void> => {
    const url = new URL(req.url ?? '/', 'http://127.0.0.1')
    const route = url.pathname.slice(API_PREFIX.length) || '/'
    if (!sameOrigin(req)) return send(res, 403, { error: { code: 'forbidden', message: 'Same-origin requests only' } })
    try {
      if (req.method === 'GET' && route === '/status') return send(res, 200, await this.status())
      if (req.method === 'GET' && route === '/morde-hub/status') return send(res, 200, await this.mordeHubStatus())
      if (req.method === 'POST' && route === '/morde-hub/login') {
        const body = await json(req)
        return send(res, 200, await this.mordeHubLogin(String(body.base_url ?? ''), String(body.name ?? ''), String(body.password ?? '')))
      }
      if (req.method === 'POST' && route === '/morde-hub/grants') return send(res, 200, await this.mordeHubUpdateGrants(await json(req)))
      if (req.method === 'POST' && route === '/morde-hub/revoke-device') {
        const body = await json(req)
        return send(res, 200, await this.mordeHubRevokeDevice(body.installation_id))
      }
      if (req.method === 'POST' && route === '/morde-hub/sign-out') return send(res, 200, await this.mordeHubSignOut())
      if (req.method === 'GET' && route === '/events') return this.stream(req, res)
      if (req.method === 'GET' && route === '/live') return send(res, 200, this.live())
      // the desktop shell's protected exit (src/work.ts): the aggregate a real quit would end,
      // the gate new work respects while it hands over, and the confirmed stop of the work
      if (req.method === 'GET' && route === '/work') return send(res, 200, { busy: this.workBusy() })
      if (req.method === 'POST' && route === '/work/gate') {
        const body = await json(req)
        return send(res, 200, { open: this.setWorkGate(body.open !== false) })
      }
      if (req.method === 'POST' && route === '/work/stop') return send(res, 200, { busy: await this.stopAllWork() })
      if (req.method === 'GET' && route === '/stage/frame') {
        // `seq` names a step's picture (0.1.40); without it, the newest
        const frame = this.trajectory.frameOf(Number(url.searchParams.get('seq') ?? 0) || 0)
        if (!frame) return send(res, 404, { error: { code: 'no_frame', message: 'Nothing on the stage' } })
        res.writeHead(200, { 'content-type': frame.mime, 'content-length': frame.bytes.length, 'cache-control': 'private, max-age=3600, immutable' })
        res.end(frame.bytes)
        return
      }
      if (req.method === 'GET' && route === '/trajectory') return send(res, 200, this.trajectoryView(url.searchParams.get('session') ?? undefined))
      if (req.method === 'POST' && route === '/stage/clear') {
        this.clearStage()
        this.trajectory.clear()
        this.broadcast()
        return send(res, 204)
      }
      if (req.method === 'POST' && route === '/code') {
        const body = await json(req)
        await this.requestCode(String(body.identifier ?? ''))
        return send(res, 204)
      }
      if (req.method === 'POST' && route === '/verify') {
        const body = await json(req)
        return send(res, 200, await this.verify(String(body.identifier ?? ''), String(body.code ?? ''), String(body.invite ?? '')))
      }
      // the account page, passed through: the sheet, the devices holding keys, the timeline, the password
      if (req.method === 'GET' && route === '/config') return send(res, 200, await this.relay.config())
      if (req.method === 'GET' && route === '/me') return send(res, 200, await this.withToken((token) => this.relay.meSheet(token)))
      if (req.method === 'GET' && route === '/sessions') return send(res, 200, await this.withToken((token) => this.relay.sessions(token)))
      if (req.method === 'POST' && route === '/sessions/revoke') {
        const body = await json(req)
        await this.withToken((token) => this.relay.revokeSession(token, String(body.prefix ?? '')))
        return send(res, 204)
      }
      if (req.method === 'GET' && route === '/account-events') {
        const limit = Number(url.searchParams.get('limit') ?? '40')
        return send(res, 200, await this.withToken((token) => this.relay.events(token, Number.isFinite(limit) ? limit : 40)))
      }
      if (req.method === 'POST' && route === '/password') {
        const body = await json(req)
        await this.withToken((token) => this.relay.setPassword(token, String(body.password ?? ''), typeof body.current === 'string' ? body.current : undefined))
        return send(res, 204)
      }
      if (req.method === 'POST' && route === '/sign-out-all') return send(res, 200, await this.endAll('sign-out-all'))
      if (req.method === 'POST' && route === '/delete-account') return send(res, 200, await this.endAll('delete'))
      if (req.method === 'POST' && route === '/login') {
        const body = await json(req)
        return send(res, 200, await this.login(String(body.identifier ?? ''), String(body.password ?? '')))
      }
      if (req.method === 'GET' && route === '/invite') {
        const invite = await this.invite()
        return invite ? send(res, 200, invite) : send(res, 404, { error: { code: 'signed_out', message: 'Sign in first' } })
      }
      if (req.method === 'POST' && route === '/profile') {
        const body = await json(req)
        const patch: Parameters<typeof this.writeProfile>[0] = {}
        if (typeof body.name === 'string') patch.name = body.name
        if (body.avatar === 'dragon' || body.avatar === 'emoji') patch.avatar = body.avatar
        if (typeof body.emoji === 'string') patch.emoji = body.emoji
        if (typeof body.color === 'string' && /^#[0-9a-fA-F]{6}$/.test(body.color)) patch.color = body.color
        return send(res, 200, await this.writeProfile(patch))
      }
      if (req.method === 'POST' && route === '/refresh') return send(res, 200, await this.refresh())
      if (req.method === 'POST' && route === '/allowance/seen') {
        await this.headsUpSeen()
        return send(res, 200, { ok: true })
      }
      if (req.method === 'GET' && route === '/studio/estimate') return send(res, 200, await this.studioEstimate())
      if (req.method === 'POST' && route === '/studio/draw') {
        const body = await json(req)
        const png = await this.studioDraw(String(body.prompt ?? '').slice(0, 2000), body.cloud === true)
        return send(res, 200, { image: png.toString('base64') })
      }
      if (req.method === 'POST' && route === '/studio/pose') {
        const body = await json(req, 8 * 1024 * 1024)
        const image = Buffer.from(String(body.image ?? ''), 'base64')
        if (!image.length) return send(res, 400, { error: { code: 'bad_request', message: 'image is the base64 PNG to pose' } })
        const png = await this.studioPose(image, String(body.prompt ?? '').slice(0, 2000), body.cloud === true)
        return send(res, 200, { image: png.toString('base64') })
      }
      if (req.method === 'POST' && route === '/studio/wear') {
        const body = await json(req, 4 * 1024 * 1024)
        const face = body.face as Record<string, string> | undefined
        if (!face || typeof face !== 'object' || typeof face.idle !== 'string') return send(res, 400, { error: { code: 'bad_request', message: 'face is a {mood: base64 WebP} map with idle' } })
        return send(res, 200, await this.wearFace(String(body.description ?? ''), String(body.style ?? 'muse'), face))
      }
      if (req.method === 'POST' && route === '/sign-out') return send(res, 200, await this.signOut())
      if (req.method === 'POST' && route === '/data/contribute') {
        const body = await json(req)
        return send(res, 200, await this.setContribute(body.on !== false))
      }
      if (req.method === 'POST' && route === '/data/delete-samples') return send(res, 200, await this.deleteSamples())
      // Conversations synced between the account's devices (C7, C8): the switch, the delete, the main chat, the rows from elsewhere.
      if (route === '/sync/state' || route.startsWith('/sync/')) {
        const sync = this.sync
        if (!sync) return send(res, 503, { error: { code: 'not_ready', message: 'The session API is not up yet' } })
        if (req.method === 'GET' && route === '/sync/state') {
          await sync.relayStatus()
          return send(res, 200, sync.view())
        }
        if (req.method === 'POST' && route === '/sync/state') {
          // the account-wide switch (`enabled`), or this device's "Also sync side chats" (`sideChats`, C9)
          const body = await json(req)
          if (typeof body.sideChats === 'boolean') return send(res, 200, await sync.setSideChats(body.sideChats))
          return send(res, 200, await sync.setEnabled(body.enabled !== false))
        }
        if (req.method === 'POST' && route === '/sync/delete') return send(res, 200, await sync.deleteRemote())
        if (req.method === 'POST' && route === '/sync/pull') return send(res, 200, { applied: await sync.pull(), ...sync.view() })
        if (req.method === 'POST' && route === '/sync/main') {
          // the session the column named, or the one the account's main conversation already lives in
          const body = await json(req)
          return send(res, 200, { sessionId: await sync.setMain(String(body.sessionId ?? '')) })
        }
        if (req.method === 'GET' && route === '/sync/remote') {
          const sessionId = url.searchParams.get('session') ?? ''
          // another account's chat shows no bubbles and no working line (C10)
          const remote = sessionId && sync.owns(sessionId) ? await this.remoteLines(this.ctx, sessionId) : { lines: [], prompts: [] }
          return send(res, 200, { lines: remote.lines, prompts: remote.prompts, hidden: sync.view().hidden, working: sync.workingOf(sessionId) })
        }
        if (req.method === 'POST' && route === '/sync/archived') {
          const body = await json(req)
          await sync.archived(Array.isArray(body.sessionIds) ? body.sessionIds.map(String) : [])
          return send(res, 204)
        }
      }
      if (req.method === 'POST' && route === '/profile/refresh') return send(res, 200, await this.pullProfile(true))
      if (req.method === 'POST' && route === '/devices/refresh') {
        this.hub.refreshDevices()
        return send(res, 204)
      }
      if (req.method === 'POST' && route === '/devices/rename') {
        const body = await json(req)
        await this.renameDevice(String(body.name ?? ''))
        return send(res, 204)
      }
      if (req.method === 'POST' && route === '/devices/forget') {
        const body = await json(req)
        this.hub.forget(String(body.device_id ?? ''))
        return send(res, 204)
      }
      if (req.method === 'POST' && route === '/devices/remote-control') {
        const body = await json(req)
        await this.setRemoteControl(body.on === true)
        return send(res, 204)
      }
      if (req.method === 'POST' && route === '/devices/trust') {
        const body = await json(req)
        await this.setTrusted(String(body.device_id ?? ''), String(body.name ?? ''), body.on === true)
        return send(res, 204)
      }
      if (req.method === 'POST' && route === '/devices/answer') {
        const body = await json(req)
        const answer = body.answer
        if (answer !== 'once' && answer !== 'always' && answer !== 'deny') return send(res, 400, { error: { code: 'bad_answer', message: 'answer must be once, always or deny' } })
        if (!this.asks.answer(String(body.id ?? ''), answer)) return send(res, 404, { error: { code: 'not_found', message: 'that question is no longer waiting' } })
        return send(res, 204)
      }
      if (req.method === 'GET' && route === '/update') return send(res, 200, await this.update(url.searchParams.get('force') === '1'))
      if (req.method === 'GET' && route === '/approvals') return send(res, 200, { approvals: this.approvalDesk.list() })
      if (req.method === 'POST' && route.startsWith('/approvals/')) {
        const body = await json(req)
        const scope = body.scope === 'schedule' || body.scope === 'conversation' || body.scope === 'always' ? body.scope : 'once'
        const ok = this.decideApproval(decodeURIComponent(route.slice('/approvals/'.length)), body.approved === true, scope)
        return ok ? send(res, 204) : send(res, 404, { error: { code: 'not_found', message: 'That question was answered already' } })
      }
      if (req.method === 'GET' && route === '/holds') return send(res, 200, { holds: this.holdDesk.list() })
      if (req.method === 'POST' && route === '/holds') {
        const body = await json(req)
        const thread = String(body.thread ?? '').trim()
        if (!thread) return send(res, 400, { error: { code: 'bad_request', message: 'thread is required' } })
        const tool: HoldTool = body.tool === 'browser' || body.tool === 'phone' ? body.tool : 'computer'
        return send(res, 200, this.takeHold(thread, tool, String(body.reason ?? '').slice(0, 200)))
      }
      if (req.method === 'POST' && /^\/holds\/[^/]+\/done$/.test(route)) {
        const id = decodeURIComponent(route.split('/')[2] ?? '')
        return this.holdDone(id) ? send(res, 204) : send(res, 404, { error: { code: 'not_found', message: 'No such hold' } })
      }
      // Standing grants (the Permissions page): every remembered permission as one list — the
      // hands' per-app grants, the devices allowed without asking, the remote-control switch —
      // and one revoke for any of them by the id the list gives.
      if (req.method === 'GET' && route === '/grants') return send(res, 200, { grants: this.standingGrants() })
      if (req.method === 'POST' && route === '/grants/revoke') {
        const body = await json(req)
        return (await this.revokeStanding(String(body.id ?? ''))) ? send(res, 204) : send(res, 404, { error: { code: 'not_found', message: 'No such grant' } })
      }
      if (req.method === 'POST' && route === '/chat-model') {
        const body = await json(req)
        await this.setChatModel(String(body.model ?? ''), typeof body.provider === 'string' && body.provider ? body.provider : PROVIDER_ID)
        return send(res, 200, this.chatChoice())
      }
      if (req.method === 'POST' && route === '/hands-model') {
        const body = await json(req)
        await this.setHandsModel(String(body.model ?? ''), typeof body.provider === 'string' && body.provider ? body.provider : PROVIDER_ID)
        return send(res, 200, this.handsChoice())
      }
      // Settings → Models (0.1.41): the four slots in one read, the two media slots' setters, the one-time Cloud retry.
      if (req.method === 'GET' && route === '/models') return send(res, 200, await this.modelsView())
      if (req.method === 'POST' && route === '/cloud-models') {
        const body = await json(req)
        await this.setCloudModels(body.on !== false)
        return send(res, 200, await this.modelsView())
      }
      if (req.method === 'POST' && route === '/image-model') {
        const body = await json(req)
        await this.setImageModel(String(body.model ?? ''), typeof body.provider === 'string' && body.provider ? body.provider : PROVIDER_ID)
        return send(res, 200, (await this.modelsView()).slots.image)
      }
      if (req.method === 'POST' && route === '/video-model') {
        const body = await json(req)
        await this.setVideoModel(String(body.model ?? ''), typeof body.provider === 'string' && body.provider ? body.provider : PROVIDER_ID)
        return send(res, 200, (await this.modelsView()).slots.video)
      }
      if (req.method === 'POST' && route === '/retry-cloud') {
        const body = await json(req)
        const sessionId = String(body.sessionId ?? '')
        if (!sessionId) return send(res, 400, { error: { code: 'bad_request', message: 'sessionId is the chat to send through nanoMuse Cloud' } })
        return send(res, 200, await this.retryOnCloud(sessionId))
      }
      // Own keys and the ChatGPT sign-in (C11): the catalogue and the rows, a key saved or removed, the pickers' options.
      if (req.method === 'GET' && route === '/providers') return send(res, 200, await this.providersView(url.searchParams.get('lang') ?? ''))
      if (req.method === 'GET' && route === '/providers/models') {
        const cap = url.searchParams.get('cap')
        if (cap === 'chat') return send(res, 200, { options: this.chatOptions() })
        if (cap === 'vision') return send(res, 200, { options: this.handsOptions() })
        if (cap === 'image') return send(res, 200, { options: this.imageOptions() })
        if (cap === 'video') return send(res, 200, { options: await this.videoOptions() })
        return send(res, 400, { error: { code: 'bad_request', message: 'cap is chat, vision, image or video' } })
      }
      if (req.method === 'POST' && route === '/providers/save') {
        const body = await json(req)
        const input: Parameters<typeof this.saveProvider>[0] = { id: String(body.id ?? '') }
        if (typeof body.row === 'string' && body.row) input.row = body.row
        if (typeof body.apiKey === 'string') input.apiKey = body.apiKey
        if (typeof body.baseURL === 'string') input.baseURL = body.baseURL
        if (typeof body.label === 'string') input.label = body.label
        if (typeof body.lang === 'string') input.lang = body.lang
        if (Array.isArray(body.capabilities)) input.capabilities = body.capabilities.map(String)
        if (Array.isArray(body.models) || typeof body.models === 'string') input.models = typedModelIds(body.models)
        if (typeof body.catalogOnly === 'boolean') input.catalogOnly = body.catalogOnly
        const row = await this.serialize(() => this.saveProvider(input))
        // the "Use it for" card: the slots this row could take and the model each would get
        return send(res, 200, { ...row, offer: this.slotOffer(row.id) })
      }
      if (req.method === 'POST' && route === '/providers/rename') {
        const body = await json(req)
        return send(res, 200, await this.serialize(() => this.renameProvider(String(body.id ?? ''), String(body.label ?? ''), typeof body.lang === 'string' ? body.lang : '')))
      }
      if (req.method === 'POST' && route === '/providers/adopt') {
        const body = await json(req)
        const slots = Array.isArray(body.slots) ? body.slots.map(String).filter((s): s is Slot => (SLOTS as readonly string[]).includes(s)) : []
        return send(res, 200, { done: await this.serialize(() => this.adoptProvider(String(body.id ?? ''), slots)) })
      }
      if (req.method === 'POST' && route === '/providers/remove') {
        const body = await json(req)
        return send(res, 200, await this.serialize(() => this.removeProvider(String(body.id ?? ''))))
      }
      if (req.method === 'POST' && route === '/chatgpt/login') return send(res, 200, { url: await this.chatGptLogin() })
      if (req.method === 'POST' && route === '/chatgpt/cancel') {
        this.chatgpt.cancelLogin()
        return send(res, 204)
      }
      if (req.method === 'POST' && route === '/chatgpt/logout') {
        await this.serialize(() => this.chatGptLogout())
        return send(res, 204)
      }
      if (req.method === 'POST' && route === '/notices/clear') {
        this.notices = []
        this.broadcast()
        return send(res, 204)
      }
      // Settings → Media and the face's clips (desk-b)
      if (req.method === 'GET' && route === '/media') return send(res, 200, await this.media())
      if (req.method === 'POST' && route === '/media') {
        const body = await json(req)
        const patch: { videoModel?: string; videoProvider?: string; animate?: boolean } = {}
        if (typeof body.videoModel === 'string') patch.videoModel = body.videoModel
        if (typeof body.videoProvider === 'string' && body.videoProvider) patch.videoProvider = body.videoProvider
        if (typeof body.animate === 'boolean') patch.animate = body.animate
        return send(res, 200, await this.setMedia(patch))
      }
      if (req.method === 'POST' && route === '/media/check') return send(res, 200, { models: await this.checkVideoModels() })
      if (req.method === 'POST' && route === '/media/animate') {
        const body = await json(req)
        const viaCloud = body.cloud === true
        if (!this.faceId()) return send(res, 409, { error: { code: 'no_face', message: 'No drawn face to animate' } })
        if (!(await this.videoEndpoint({ cloud: viaCloud }))) {
          const signedOut = viaCloud && relayMediaEnabled(this.relay.origin) && (!this.signedInCache || !this.state.account)
          return send(res, signedOut ? 401 : 409, { error: { code: signedOut ? 'signed_out' : 'no_video_model', message: signedOut ? 'Sign in to use the relay' : 'No video model to draw clips with' } })
        }
        const started = await this.motion.animateAll(body.force === true, viaCloud)
        return send(res, 200, { started, motion: this.motion.view() })
      }
      if (req.method === 'POST' && route === '/media/cancel') {
        this.motion.cancel()
        return send(res, 204)
      }
      if (req.method === 'POST' && route === '/media/dismiss') {
        this.motion.clearProgress()
        return send(res, 204)
      }
      // A clip of the person's own as the face (#324): the picked file comes as the body, is looked at and
      // kept as a pending copy for the preview; "Use" makes it the clip, "Cancel" drops it, "Default face" removes it.
      if (req.method === 'POST' && route === '/own-clip/stage') {
        const staged = await this.ownClip.stage(req)
        if (staged.ok) return send(res, 200, staged)
        const seconds = Math.floor((staged.probe.durationMs ?? 0) / 1000)
        return send(res, staged.why === 'too_large' ? 413 : 415, { error: { code: `clip_${staged.why}`, message: `The clip was refused: ${staged.why}` }, seconds, bytes: staged.probe.bytes })
      }
      if (req.method === 'POST' && route === '/own-clip/use') {
        const body = await json(req, 2 * 1024 * 1024)
        if (body.mood != null && !isOwnClipMood(body.mood)) return send(res, 400, { error: { code: 'bad_mood', message: 'Unknown avatar mood' } })
        const poster = typeof body.poster === 'string' && body.poster ? Buffer.from(body.poster, 'base64') : undefined
        if (!(await this.ownClip.use(poster, body.mood as OwnClipMood | undefined))) return send(res, 409, { error: { code: 'no_pending', message: 'No clip waiting to be used' } })
        return send(res, 200, { ownClip: this.ownClip.view() })
      }
      if (req.method === 'POST' && route === '/own-clip/discard') {
        await this.ownClip.discard()
        return send(res, 204)
      }
      if (req.method === 'POST' && route === '/own-clip/clear') {
        const body: Record<string, unknown> = await json(req).catch(() => ({}))
        if (body.mood != null && !isOwnClipMood(body.mood)) return send(res, 400, { error: { code: 'bad_mood', message: 'Unknown avatar mood' } })
        await this.ownClip.clear(body.mood as OwnClipMood | undefined)
        return send(res, 204)
      }
      // Settings → Computer use: the runtime and the "try it" checks (desk-b)
      if (req.method === 'GET' && route === '/hands/runtime') return send(res, 200, await this.runtime())
      if (req.method === 'POST' && route === '/hands/check') {
        const body = await json(req)
        const kind = body.kind === 'move' ? 'move' : 'screenshot'
        return send(res, 200, await this.handsCheck(kind))
      }
      if (req.method === 'POST' && route === '/hands/black-screen/clear') {
        this.blackScreenAt = 0
        this.broadcast()
        return send(res, 204)
      }
      // Settings → Coding agents: this computer's agents and, with `device`, another computer's over the hub
      if (route === '/coding' || route.startsWith('/coding/')) {
        if (await this.coding.http(route.slice('/coding'.length), req, res, url, () => json(req), send)) return
      }
      return send(res, 404, { error: { code: 'not_found', message: `No ${req.method ?? ''} ${route}` } })
    } catch (error: unknown) {
      if (error instanceof RelayError) {
        return send(res, error.status >= 500 ? 502 : error.status, { error: { code: error.code, message: error.message } })
      }
      // the wire, not a refusal: a relay out of reach or past its deadline gets a code the
      // browser half puts into words, instead of `fetch failed` shown as it came
      const transport = transportFailure(error)
      if (transport) {
        this.ctx.logger.warn('nanomuse cloud: %s %s: %s (%s)', req.method, route, transport, message(error))
        return send(res, transport === 'timeout' ? 504 : 503, { error: { code: transport, message: message(error) } })
      }
      this.ctx.logger.warn('nanomuse cloud: %s %s failed: %s', req.method, route, message(error))
      return send(res, 500, { error: { code: 'internal', message: message(error) } })
    }
  }
}

/**
 * The `nanomuse` provider row for a relay's OpenAI root and its model list (see `providerRow`).
 * `deviceId` rides on every chat call as `X-NanoMuse-Device` (relay 0.29, docs/cloud.md), so
 * the record says which device a turn came from; the adapter sends a provider's headers as
 * they are, so the device is named here once rather than per request.
 */
export function providerRowFor(openaiBase: string, models: RelayModel[], deviceId = ''): Record<string, unknown> {
  const chat = models.filter((m) => modelFor(m).includes('chat'))
  return {
    displayName: 'nanoMuse Cloud',
    api: 'openai-completions',
    baseURL: openaiBase,
    apiKeyEnv: TOKEN_REF,
    ...(deviceId ? { headers: { 'x-nanomuse-device': deviceId } } : {}),
    ...IMAGE_BUDGET,
    models: chat.map((m) => ({
      id: m.id,
      displayName: m.name,
      input: takesImages(m) ? ['text', 'image'] : ['text'],
    })),
  }
}

/** `$DSH_HOME`, or `~/.dsh`: the same rule the launcher applies. */
export function dshHome(): string {
  const configured = process.env.DSH_HOME
  if (configured) return configured
  return join(process.env.HOME ?? process.env.USERPROFILE ?? '.', '.dsh')
}

/** The reason of a `hand_over` action (the agent gives the screen to the person), or nothing. */
export function handOverOf(args: unknown): string | undefined {
  if (!args || typeof args !== 'object') return undefined
  const a = args as Record<string, unknown>
  if (a.action !== 'hand_over') return undefined
  return String(a.reason ?? a.label ?? a.text ?? '').trim().slice(0, 200) || 'Your turn'
}

/** The hands' arguments as the stage shows them: what kind of step, aimed where. */
export function stageAction(args: unknown): StageAction {
  const a = (args && typeof args === 'object' ? args : {}) as Record<string, unknown>
  const num = (v: unknown): number => (typeof v === 'number' && Number.isFinite(v) ? v : typeof v === 'string' && v.trim() && Number.isFinite(Number(v)) ? Number(v) : -1)
  const kind = typeof a.action === 'string' ? a.action : 'act'
  const keys = Array.isArray(a.keys) ? a.keys.filter((k): k is string => typeof k === 'string').join('+') : ''
  const text = kind === 'key' ? keys : kind === 'open_app' ? String(a.app ?? '') : typeof a.text === 'string' ? a.text : ''
  // `box: [x1, y1, x2, y2]` stands in for x, y when the model gave a box (computer_act): its centre; `box2` the same for a drag's end
  const centre = (raw: unknown, x: unknown, y: unknown): [number, number] => {
    const box = Array.isArray(raw) && raw.length === 4 ? raw.map(num) : []
    const [bx1 = -1, by1 = -1, bx2 = -1, by2 = -1] = box
    const boxed = box.length === 4 && box.every((v) => v >= 0)
    return [boxed && x === undefined ? (bx1 + bx2) / 2 : num(x), boxed && y === undefined ? (by1 + by2) / 2 : num(y)]
  }
  const [x, y] = centre(a.box, a.x, a.y)
  const [x2, y2] = kind === 'drag' ? centre(a.box2, a.x2, a.y2) : [-1, -1]
  // the runtime scrolls 300 px down when the model names no amount
  const dy = kind === 'scroll' ? (a.dy === undefined ? 300 : num(a.dy)) : 0
  return {
    kind,
    label: typeof a.label === 'string' ? a.label.replace(/\s+/g, ' ').trim().slice(0, 80) : '',
    text: text.replace(/\s+/g, ' ').trim().slice(0, 80),
    x,
    y,
    x2,
    y2,
    dy,
    at: Date.now(),
  }
}

/**
 * The user messages the harness adds to a turn on its own (the time, the runtime, the skills): they
 * do not end a scheduled run. Any other kind is someone writing, and the run is theirs from then on.
 */
const HARNESS_CONTEXT_KINDS = new Set(['time-context', 'runtime-context', 'skill-catalog', 'agent-instructions', 'model-selection', 'tool-jobs', 'tool-registry', 'system-prompt', 'ptc-mode', 'user-approval'])

/** The grant target 0.1.37 wrote when it took `computer_act`'s first line for the window title. */
export const MISREAD_GRANT = /^computer_app:Done\. Screen now:?$/

/** The screen head reader lives with the hands' checks (hands-check.ts); the tests import it from here. */
export { screenHead }

/** The few arguments the capsule shows, as short strings. */
export function pickArgs(args: unknown): Record<string, string> {
  const out: Record<string, string> = {}
  if (!args || typeof args !== 'object') return out
  const record = args as Record<string, unknown>
  for (const key of ARG_KEYS) {
    const value = record[key]
    if (typeof value === 'string' && value.trim()) out[key] = value.replace(/\s+/g, ' ').trim().slice(0, 80)
    else if (typeof value === 'number') out[key] = String(value)
  }
  return out
}

function sameModels(a: RelayModel[], b: RelayModel[]): boolean {
  const key = (models: RelayModel[]) =>
    models
      .filter((m) => m.kind === 'chat')
      .map((m) => `${m.id}:${m.name}:${m.inputModalities.join(',')}`)
      .sort()
      .join('|')
  return key(a) === key(b)
}

/** A browser on another origin cannot sign this device in or out. */
export function sameOrigin(req: IncomingMessage): boolean {
  const origin = req.headers.origin
  const site = req.headers['sec-fetch-site']
  if (typeof site === 'string' && site !== 'same-origin' && site !== 'none') return false
  if (typeof origin !== 'string') return true
  const host = req.headers.host
  return typeof host === 'string' && (origin === `http://${host}` || origin === `https://${host}`)
}

/** The request body as an object; empty when there is none. */
export async function json(req: IncomingMessage, limit = 64 * 1024): Promise<Record<string, unknown>> {
  const chunks: Buffer[] = []
  let size = 0
  for await (const chunk of req) {
    const buffer = chunk as Buffer
    size += buffer.length
    if (size > limit) throw new RelayError(413, 'too_large', 'Request body too large')
    chunks.push(buffer)
  }
  if (chunks.length === 0) return {}
  try {
    const parsed: unknown = JSON.parse(Buffer.concat(chunks).toString('utf8'))
    return parsed && typeof parsed === 'object' ? (parsed as Record<string, unknown>) : {}
  } catch {
    throw new RelayError(400, 'bad_json', 'The request body is not JSON')
  }
}

/** A JSON reply, or an empty one for 204. */
export function send(res: ServerResponse, status: number, body?: unknown): void {
  if (body === undefined) {
    res.writeHead(status, { 'cache-control': 'no-store' }).end()
    return
  }
  res.writeHead(status, { 'content-type': 'application/json', 'cache-control': 'no-store' }).end(JSON.stringify(body))
}

export function message(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
}
