#!/usr/bin/env node

import { createHash } from 'node:crypto'
import { readFile, rename, rm, writeFile } from 'node:fs/promises'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const expectedPackage = '@deepseek-ai/dsh-agent-loop'
const expectedVersion = '0.2.2-rc.1'
const expectedSourceHash = 'a6fa8e0c7c90191caf5f4a3bb5c624a86e9845cf3c79d020b71420d0e20ae4c1'
const expectedPatchedHash = '45ae9c88b379ea89157a9131eded1ee716f82859ed47221e8de55185df95c8ca'

function sha256(value) {
  return createHash('sha256').update(value).digest('hex')
}

function replaceOnce(source, before, after, label) {
  const first = source.indexOf(before)
  if (first < 0 || source.indexOf(before, first + before.length) >= 0) {
    throw new Error(`expected one ${label} anchor in ${expectedPackage}`)
  }
  return `${source.slice(0, first)}${after}${source.slice(first + before.length)}`
}

const runtimeRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const cordisRoot = join(runtimeRoot, 'node_modules', '@deepseek-ai', 'cordis')
const cordisPackageJson = JSON.parse(await readFile(join(cordisRoot, 'package.json'), 'utf8'))
if (cordisPackageJson.name !== '@deepseek-ai/cordis' || cordisPackageJson.version !== '4.1.0') {
  throw new Error(`refusing to patch: expected @deepseek-ai/cordis@4.1.0, found ${cordisPackageJson.name}@${cordisPackageJson.version}`)
}
const cordisSource = await readFile(join(cordisRoot, 'lib', 'index.js'), 'utf8')
const requiredCordisStructures = [
  'var EventsService = class {\n\tctx;\n\t_hooks = Object.create(null);',
  '_resolve(type, args) {',
  'const filter = thisArg?.[Context.filter];',
  'waterfall(...args) {\n\t\tconst [thisArg, callbacks] = this._resolve("waterfall", args);',
  '(this._hooks[name] ??= [])[method]({',
  'this.events = new EventsService(self);',
]
if (requiredCordisStructures.some((structure) => !cordisSource.includes(structure))) {
  throw new Error('refusing to patch: @deepseek-ai/cordis@4.1.0 lacks the expected EventsService hook registry or waterfall API structure')
}
const packageRoot = join(runtimeRoot, 'node_modules', '@deepseek-ai', 'dsh-agent-loop')
const packageJson = JSON.parse(await readFile(join(packageRoot, 'package.json'), 'utf8'))
if (packageJson.name !== expectedPackage || packageJson.version !== expectedVersion || packageJson.license !== 'MIT') {
  throw new Error(`refusing to patch unexpected ${packageJson.name}@${packageJson.version} (${packageJson.license})`)
}
const license = await readFile(join(packageRoot, 'LICENSE'), 'utf8')
if (!license.startsWith('MIT License') || !license.includes('Permission is hereby granted')) {
  throw new Error(`refusing to patch ${expectedPackage}: upstream MIT LICENSE is missing or changed`)
}

const sourcePath = join(packageRoot, 'lib', 'index.js')
const source = await readFile(sourcePath, 'utf8')
const sourceHash = sha256(source)
if (sourceHash === expectedPatchedHash) {
  process.stdout.write(`${expectedPackage}@${expectedVersion}: request-budget hook already applied\n`)
  process.exit(0)
}
if (sourceHash !== expectedSourceHash) {
  throw new Error(`refusing to patch ${expectedPackage}@${expectedVersion}: unexpected lib/index.js SHA-256 ${sourceHash}`)
}

const stepAnchor = `\t\t\tconst request = this.buildRequest(config, preparedCall, assembly.tools, {
				turn,
				step
			}, startsRequestSeries, signal);
			const live = new AssistantStreamAttempt`
const stepReplacement = `\t\t\tconst builtRequest = this.buildRequest(config, preparedCall, assembly.tools, {
				turn,
				step
			}, startsRequestSeries, signal);
			const built = builtRequest && typeof builtRequest.then === "function" ? await builtRequest : builtRequest;
			const { request, preparedCall: requestPreparedCall } = built;
			const live = new AssistantStreamAttempt`

let patched = replaceOnce(source, stepAnchor, stepReplacement, 'step request assembly')
patched = replaceOnce(
  patched,
  'const stream = preparedCall?.stream(request) ?? this.loopCtx.llm.stream(request);',
  'const stream = requestPreparedCall?.stream(request) ?? this.loopCtx.llm.stream(request);',
  'prepared stream dispatch'
)
patched = replaceOnce(
  patched,
  'retryPolicy: preparedCall?.retryPolicy,',
  'retryPolicy: requestPreparedCall?.retryPolicy,',
  'prepared request retry policy'
)

const methodAnchor = `\t/** Log the resolved envelope and derive a frozen request from the admitted surface. */
	buildRequest(config, preparedCall, tools, position, startsRequestSeries, signal) {
		const { session } = this;`
const methodReplacement = `\t/**
	* Offer the fully projected request surface to an optional per-step budget hook.
	* Cordis 4.1.0 exposes the hook registry; the empty-hook path keeps the original
	* request builder and its side effects unchanged.
	*/
	buildRequest(config, preparedCall, tools, position, startsRequestSeries, signal) {
		if (!this.ctx.events._hooks["agent/request-budget"]?.length) return {
			request: this.buildRequestNow(config, preparedCall, tools, position, startsRequestSeries, signal),
			preparedCall
		};
		const draft = this.previewRequestBudget(config, preparedCall, tools, position, startsRequestSeries, signal);
		const result = this.dispatch.waterfall("agent/request-budget", draft.payload, () => void 0);
		const finish = (decision) => {
			if (decision === void 0) return {
				request: this.buildRequestNow(config, preparedCall, tools, position, startsRequestSeries, signal, draft.toolRegistryMessage),
				preparedCall
			};
			if (decision === null || typeof decision !== "object" || Array.isArray(decision) || Object.getPrototypeOf(decision) !== Object.prototype && Object.getPrototypeOf(decision) !== null || Object.keys(decision).length !== 1 || !Object.hasOwn(decision, "maxTokens") || !Number.isSafeInteger(decision.maxTokens) || decision.maxTokens <= 0) throw new TypeError('agent/request-budget must return undefined or { maxTokens: positive safe integer }');
			if (decision.maxTokens === config.maxTokens) return {
				request: this.buildRequestNow(config, preparedCall, tools, position, startsRequestSeries, signal, draft.toolRegistryMessage),
				preparedCall
			};
			signal.throwIfAborted();
			const cappedConfig = { ...config, maxTokens: decision.maxTokens };
			if (preparedCall === void 0) return {
				request: this.buildRequestNow(cappedConfig, void 0, tools, position, startsRequestSeries, signal, draft.toolRegistryMessage),
				preparedCall: void 0
			};
			return this.loopCtx.llm.prepareCall(cappedConfig, signal).then((cappedCall) => {
				signal.throwIfAborted();
				const withoutCap = (value) => {
					const { maxTokens: _maxTokens, ...rest } = value;
					return JSON.stringify(rest);
				};
				if (cappedCall.config.provider !== config.provider || cappedCall.config.model !== config.model || cappedCall.config.reasoningEffort !== config.reasoningEffort || cappedCall.config.maxTokens !== decision.maxTokens || withoutCap(cappedCall.config) !== withoutCap(config)) throw new Error('agent/request-budget changed the prepared request route or controls');
				for (const field of ["context", "inputModalities", "systemPromptUpdate", "toolUpdate", "retryPolicy"]) if (JSON.stringify(cappedCall[field]) !== JSON.stringify(preparedCall[field])) throw new Error('agent/request-budget changed prepared model metadata');
				return {
					request: this.buildRequestNow(cappedCall.config, cappedCall, tools, position, startsRequestSeries, signal, draft.toolRegistryMessage),
					preparedCall: cappedCall
				};
			});
		};
		return result !== null && typeof result === "object" && typeof result.then === "function" ? Promise.resolve(result).then(finish) : finish(result);
	}
	/** Build an immutable preview matching the request the ordinary builder will commit. */
	previewRequestBudget(config, preparedCall, tools, position, startsRequestSeries, signal) {
		const { session } = this;
		const surfaceGeneration = session.surface.contentGeneration;
		const header = canonicalHeader({
			config,
			...preparedCall === void 0 ? {} : { adapterDefaults: preparedCall.adapterDefaults },
			...tools.length > 0 ? { tools } : {}
		});
		const baseline = session.requestHeader();
		const startsSeries = startsRequestSeries || this.requestSurfaceGeneration !== surfaceGeneration;
		const appendsHeader = !this.requestHeaderLogged || baseline === void 0 || !headerEquals(baseline, header);
		let toolRegistryMessage;
		let additions = [];
		let removals = [];
		if (baseline !== void 0 && appendsHeader) {
			const previousNames = new Set(baseline.tools?.map((tool) => tool.name));
			const currentNames = new Set(tools.map((tool) => tool.name));
			additions = tools.filter((tool) => !previousNames.has(tool.name)).map((tool) => ({ type: "tool-addition", toolName: tool.name }));
			removals = (baseline.tools ?? []).filter((tool) => !currentNames.has(tool.name)).map((tool) => ({ type: "tool-removal", toolName: tool.name }));
			if (additions.length > 0 || removals.length > 0) toolRegistryMessage = createDeveloperMessage({
				source: { kind: "tool-registry" },
				content: [...additions, ...removals]
			});
		}
		const messages = session.deriveMessages();
		if (toolRegistryMessage !== void 0) messages.push(toolRegistryMessage);
		const history = session.toolHistory();
		const activeTools = header.tools ?? [];
		let projectedHistory = history;
		let available = new Set((baseline?.tools ?? history.tools).map((tool) => tool.name));
		const declared = new Map(history.tools.map((tool) => [tool.name, tool]));
		for (const update of history.updates) for (const tool of update.additions) declared.set(tool.name, tool);
		const redeclared = activeTools.some((tool) => declared.has(tool.name) && JSON.stringify(declared.get(tool.name)) !== JSON.stringify(tool));
		const resetsHistory = baseline === void 0 || startsSeries || appendsHeader && redeclared;
		if (resetsHistory) {
			projectedHistory = { tools: activeTools, updates: [] };
			available = new Set(activeTools.map((tool) => tool.name));
		} else if (appendsHeader) available = new Set((baseline?.tools ?? history.tools).map((tool) => tool.name));
		if (toolRegistryMessage !== void 0) {
			const addedTools = additions.map((item) => activeTools.find((tool) => tool.name === item.toolName)).filter((tool) => tool !== void 0);
			for (const item of additions) available.add(item.toolName);
			for (const item of removals) available.delete(item.toolName);
			projectedHistory = {
				tools: projectedHistory.tools,
				updates: [...projectedHistory.updates, { messageId: toolRegistryMessage.id, additions: addedTools }]
			};
		}
		const toolHistory = activeTools.length !== available.size || activeTools.some((tool) => !available.has(tool.name)) ? { tools: activeTools, updates: [] } : projectedHistory;
		return {
			toolRegistryMessage,
			payload: Object.freeze({
				turn: position.turn,
				step: position.step,
				signal,
				config: deepFreeze(structuredClone(config)),
				messages: deepFreeze(structuredClone(messages)),
				tools: deepFreeze(structuredClone(header.tools)),
				toolHistory: deepFreeze(structuredClone(toolHistory))
			})
		};
	}
	/** Log the resolved envelope and derive a frozen request from the admitted surface. */
	buildRequestNow(config, preparedCall, tools, position, startsRequestSeries, signal, draftToolRegistryMessage) {
		const { session } = this;`
patched = replaceOnce(patched, methodAnchor, methodReplacement, 'request builder insertion')

patched = replaceOnce(
  patched,
  'message: createDeveloperMessage({\n\t\t\t\t\tsource: { kind: "tool-registry" },\n\t\t\t\t\tcontent: [...additions, ...removals]\n\t\t\t\t}),',
  'message: draftToolRegistryMessage ?? createDeveloperMessage({\n\t\t\t\t\tsource: { kind: "tool-registry" },\n\t\t\t\t\tcontent: [...additions, ...removals]\n\t\t\t\t}),',
  'tool registry message reuse'
)

const patchedHash = sha256(patched)
if (patchedHash !== expectedPatchedHash) {
  throw new Error(`local patch verification failed; computed patched SHA-256 ${patchedHash}`)
}

const temporaryPath = `${sourcePath}.request-budget.tmp`
try {
  await writeFile(temporaryPath, patched, { flag: 'wx' })
  await rename(temporaryPath, sourcePath)
} catch (error) {
  await rm(temporaryPath, { force: true })
  throw error
}
process.stdout.write(`${expectedPackage}@${expectedVersion}: applied request-budget hook (${patchedHash})\n`)
