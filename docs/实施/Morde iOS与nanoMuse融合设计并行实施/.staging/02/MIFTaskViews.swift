import SwiftUI

enum MIFTaskPageRules {
    static func merge(current: [MordeTask], incoming: [MordeTask]) -> [MordeTask] {
        var merged: [MordeTask] = []
        var indexByID: [String: Int] = [:]
        for task in current + incoming {
            if let index = indexByID[task.id] {
                merged[index] = task
            } else {
                indexByID[task.id] = merged.count
                merged.append(task)
            }
        }
        return merged
    }

    static func canLoadMore(cursor: String?, isLoading: Bool) -> Bool {
        !isLoading && cursor.map { !$0.isEmpty } == true
    }

    static func hasValidNextCursor(requested: String?, returned: String?) -> Bool {
        guard let returned else { return true }
        return !returned.isEmpty && returned != requested
    }
}

enum MIFTaskActionKind: String {
    case pause
    case resume
    case cancel
    case retryBatchItem
}

struct MIFTaskActionOption: Identifiable {
    let id: String
    let kind: MIFTaskActionKind
    let title: String
    let systemImage: String
    let action: MordeTaskPageAction
}

enum MIFTaskActionRules {
    static func options(for task: MordeTask) -> [MIFTaskActionOption] {
        switch task.status {
        case "queued", "running":
            return [
                MIFTaskActionOption(id: "pause", kind: .pause, title: AppLocalized("Pause"),
                                    systemImage: "pause.fill", action: .pause(expectedStatus: task.status)),
                MIFTaskActionOption(id: "cancel", kind: .cancel, title: AppLocalized("Cancel task"),
                                    systemImage: "xmark", action: .cancel(expectedStatus: task.status)),
            ]
        case "paused":
            return [
                MIFTaskActionOption(id: "resume", kind: .resume, title: AppLocalized("Resume"),
                                    systemImage: "play.fill", action: .resume),
                MIFTaskActionOption(id: "cancel", kind: .cancel, title: AppLocalized("Cancel task"),
                                    systemImage: "xmark", action: .cancel(expectedStatus: task.status)),
            ]
        case "waiting_user":
            return [
                MIFTaskActionOption(id: "cancel", kind: .cancel, title: AppLocalized("Cancel task"),
                                    systemImage: "xmark", action: .cancel(expectedStatus: task.status)),
            ]
        case "failed":
            return (task.batch_items ?? []).filter {
                canRetryMordeBatchItem(taskStatus: task.status, itemStatus: $0.status)
            }.map {
                MIFTaskActionOption(id: "retry:\($0.id)", kind: .retryBatchItem,
                                    title: AppLocalized("Retry batch item: \($0.key)"), systemImage: "arrow.clockwise",
                                    action: .retryBatchItem(id: $0.id))
            }
        default:
            return []
        }
    }
}

enum MIFTaskPresentation {
    static func visibleSteps(_ steps: [MordeTask.Step]?) -> [MordeTask.Step] {
        (steps ?? []).filter { $0.kind != "budget" }.sorted { $0.sequence < $1.sequence }
    }

    static func stepStatusLabel(_ status: String) -> String {
        switch status {
        case "queued": return AppLocalized("Queued")
        case "running": return AppLocalized("Running")
        case "waiting_user": return AppLocalized("Waiting for you")
        case "succeeded": return AppLocalized("Completed")
        case "failed": return AppLocalized("Failed")
        case "unknown": return AppLocalized("Status unknown")
        default: return AppLocalized("Status unknown")
        }
    }

    static func timelineRows(_ events: [MordeTaskTimelineEvent]) -> [MIFTaskTimelineRow] {
        var seen = Set<Int64>()
        return events.sorted { $0.sequence < $1.sequence }.compactMap { event in
            guard seen.insert(event.sequence).inserted, let display = event.display else { return nil }
            return MIFTaskTimelineRow(id: "event:\(event.sequence)", sequence: event.sequence,
                                      title: display.title, summary: display.summary)
        }
    }
}

struct MIFTaskTimelineRow: Identifiable {
    let id: String
    let sequence: Int64
    let title: String
    let summary: String?
}

@MainActor
struct MIFTaskListRoom: View {
    @StateObject private var store = MIFTaskPageStore.shared
    @Environment(\.dismiss) private var dismiss
    @State private var tasks: [MordeTask] = []
    @State private var nextCursor: String?
    @State private var pageError: String?
    @State private var isLoadingInitial = false
    @State private var isLoadingMore = false
    @State private var didLoadInitial = false
    @State private var requestGeneration = UUID()

    var body: some View {
        VStack(spacing: 0) {
            NanoMuseTopBar(title: AppLocalized("Task"), onBack: { dismiss() }) {
                Button {
                    Task { await beginScope(scope: store.scopeKey) }
                } label: {
                    Image(systemName: "arrow.clockwise")
                        .font(.system(size: 16, weight: .medium))
                        .foregroundStyle(NanoMuseTones.action)
                        .frame(width: 40, height: 40)
                        .contentShape(Circle())
                }
                .accessibilityLabel(AppLocalized("Refresh"))
            }

            ScrollView {
                VStack(spacing: 14) {
                    listContent
                }
                .padding(.vertical, 14)
            }
            .background(NanoMuseTones.canvas)
        }
        .background(NanoMuseTones.canvas.ignoresSafeArea())
        .toolbar(.hidden, for: .navigationBar)
        .task(id: store.scopeKey) {
            await beginScope(scope: store.scopeKey)
        }
        .tint(NanoMuseTones.action)
    }

    @ViewBuilder
    private var listContent: some View {
        if !store.isSignedIn {
            MIFTaskNotice(text: AppLocalized("Sign in to view Morde tasks"), symbol: "person.crop.circle")
        } else if store.scopeKey == nil {
            MIFTaskNotice(text: AppLocalized("Confirming account scope…"), symbol: "arrow.triangle.2.circlepath")
        } else if tasks.isEmpty && isLoadingInitial {
            ProgressView(AppLocalized("Loading tasks…"))
                .frame(maxWidth: .infinity, minHeight: 150)
        } else if tasks.isEmpty, let pageError {
            MIFTaskErrorState(message: pageError) {
                Task { await beginScope(scope: store.scopeKey) }
            }
        } else if tasks.isEmpty && didLoadInitial {
            MIFTaskNotice(text: AppLocalized("No tasks yet"), symbol: "checklist")
        } else if tasks.isEmpty {
            MIFTaskNotice(text: AppLocalized("Task list has not loaded"), symbol: "checklist")
        } else {
            if let pageError {
                MIFTaskInlineError(message: pageError) {
                    Task { await loadMoreTasks() }
                }
            }
            NanoMuseCard {
                ForEach(Array(tasks.enumerated()), id: \.element.id) { index, task in
                    if index > 0 { NanoMuseRowDivider() }
                    NavigationLink {
                        MIFTaskDetailView(taskID: task.id)
                    } label: {
                        MIFTaskListRow(task: store.details[task.id] ?? task)
                    }
                    .buttonStyle(.plain)
                    .accessibilityIdentifier("morde.tasks.row.\(task.id)")
                }
            }

            if nextCursor != nil {
                Button {
                    Task { await loadMoreTasks() }
                } label: {
                    if isLoadingMore {
                        ProgressView()
                            .frame(maxWidth: .infinity, minHeight: 44)
                    } else {
                        Text(AppLocalized("Load more tasks"))
                            .font(.subheadline.weight(.medium))
                            .frame(maxWidth: .infinity, minHeight: 44)
                    }
                }
                .disabled(isLoadingMore || isLoadingInitial)
                .accessibilityIdentifier("morde.tasks.loadMore")
            }
        }
    }

    private func beginScope(scope: String?) async {
        let generation = UUID()
        requestGeneration = generation
        tasks = []
        nextCursor = nil
        pageError = nil
        didLoadInitial = false
        isLoadingInitial = false
        isLoadingMore = false
        guard store.isSignedIn, let scope else { return }
        await fetchPage(scope: scope, generation: generation, cursor: nil, appending: false)
    }

    private func loadMoreTasks() async {
        guard MIFTaskPageRules.canLoadMore(cursor: nextCursor, isLoading: isLoadingMore || isLoadingInitial),
              let scope = store.scopeKey else { return }
        await fetchPage(scope: scope, generation: requestGeneration, cursor: nextCursor, appending: true)
    }

    private func fetchPage(scope: String, generation: UUID, cursor: String?, appending: Bool) async {
        if appending { isLoadingMore = true } else { isLoadingInitial = true }
        defer {
            if generation == requestGeneration {
                isLoadingInitial = false
                isLoadingMore = false
            }
        }
        do {
            let page = try await store.taskPage(cursor: cursor)
            guard !Task.isCancelled, generation == requestGeneration, scope == store.scopeKey else { return }
            guard MIFTaskPageRules.hasValidNextCursor(requested: cursor, returned: page.next_cursor) else {
                throw URLError(.cannotParseResponse)
            }
            tasks = MIFTaskPageRules.merge(current: appending ? tasks : [], incoming: page.items)
            nextCursor = page.next_cursor
            pageError = nil
            didLoadInitial = true
        } catch {
            guard !Task.isCancelled, generation == requestGeneration, scope == store.scopeKey else { return }
            pageError = error.localizedDescription
        }
    }
}

typealias MIFTaskRoom = MIFTaskListRoom

@MainActor
struct MIFTaskDetailView: View {
    let taskID: String
    @StateObject private var store = MIFTaskPageStore.shared
    @Environment(\.dismiss) private var dismiss
    @State private var loadedScopeKey: String?
    @State private var detailError: String?
    @State private var isLoadingDetail = false
    @State private var isPerformingAction = false
    @State private var confirmsCancel = false
    @State private var detailGeneration = UUID()

    private var task: MordeTask? {
        guard loadedScopeKey == store.scopeKey else { return nil }
        return store.details[taskID]
    }

    var body: some View {
        VStack(spacing: 0) {
            NanoMuseTopBar(title: AppLocalized("Task"), onBack: { dismiss() })
            ScrollView {
                VStack(spacing: 16) {
                    detailContent
                }
                .padding(.vertical, 14)
            }
            .background(NanoMuseTones.canvas)
        }
        .background(NanoMuseTones.canvas.ignoresSafeArea())
        .toolbar(.hidden, for: .navigationBar)
        .confirmationDialog(AppLocalized("Cancel this task?"), isPresented: $confirmsCancel,
                            titleVisibility: .visible) {
            Button(AppLocalized("Cancel task"), role: .destructive) {
                guard let option = actionOptions.first(where: { $0.kind == .cancel }) else { return }
                Task { await perform(option) }
            }
            Button(AppLocalized("Back"), role: .cancel) {}
        }
        .task(id: store.scopeKey) {
            await loadDetail(scope: store.scopeKey)
        }
    }

    @ViewBuilder
    private var detailContent: some View {
        if !store.isSignedIn {
            MIFTaskNotice(text: AppLocalized("Sign in to view Morde tasks"), symbol: "person.crop.circle")
        } else if isLoadingDetail {
            ProgressView(AppLocalized("Loading tasks…"))
                .frame(maxWidth: .infinity, minHeight: 150)
        } else if let detailError {
            MIFTaskErrorState(message: detailError) {
                Task { await loadDetail(scope: store.scopeKey) }
            }
        } else if let task {
            taskSummary(task)
            taskActions(task)
            taskSteps(task)
            taskActivity(task)
            taskArtifacts(task)
        } else {
            MIFTaskNotice(text: AppLocalized("Task details unavailable"), symbol: "exclamationmark.circle")
        }
    }

    private var actionOptions: [MIFTaskActionOption] {
        guard let task else { return [] }
        return MIFTaskActionRules.options(for: task)
    }

    private func taskSummary(_ task: MordeTask) -> some View {
        MIFTaskSection(title: AppLocalized("Task")) {
            VStack(alignment: .leading, spacing: 12) {
                Text(task.objective)
                    .font(.title3.weight(.semibold))
                    .foregroundStyle(.primary)
                    .fixedSize(horizontal: false, vertical: true)
                    .accessibilityIdentifier("morde.task.objective")
                Text(task.statusLabel)
                    .font(.subheadline.weight(.medium))
                    .padding(.horizontal, 10)
                    .padding(.vertical, 6)
                    .background(NanoMuseTones.fill, in: Capsule())
                    .accessibilityIdentifier("morde.task.status")
                if let currentStep = task.current_step, !currentStep.isEmpty {
                    VStack(alignment: .leading, spacing: 4) {
                        Text(AppLocalized("Current step"))
                            .font(.caption.weight(.medium))
                            .foregroundStyle(.secondary)
                        Text(currentStep)
                            .font(.body)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                    .accessibilityIdentifier("morde.task.currentStep")
                }
                if let result = task.result, !result.isEmpty {
                    VStack(alignment: .leading, spacing: 4) {
                        Text(AppLocalized("Result"))
                            .font(.caption.weight(.medium))
                            .foregroundStyle(.secondary)
                        Text(result)
                            .font(.body)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                    .accessibilityIdentifier("morde.task.result")
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(16)
        }
    }

    @ViewBuilder
    private func taskActions(_ task: MordeTask) -> some View {
        let options = MIFTaskActionRules.options(for: task)
        if !options.isEmpty {
            MIFTaskSection(title: AppLocalized("Task actions")) {
                VStack(alignment: .leading, spacing: 0) {
                    ForEach(Array(options.enumerated()), id: \.element.id) { index, option in
                        if index > 0 { NanoMuseRowDivider() }
                        Button {
                            if option.kind == .cancel {
                                confirmsCancel = true
                            } else {
                                Task { await perform(option) }
                            }
                        } label: {
                            HStack(spacing: 12) {
                                Image(systemName: option.systemImage)
                                    .frame(width: 22)
                                Text(option.title)
                                Spacer()
                                if isPerformingAction { ProgressView().controlSize(.small) }
                            }
                            .foregroundStyle(option.kind == .cancel ? Color.red : Color.primary)
                            .padding(.horizontal, 16)
                            .padding(.vertical, 13)
                            .contentShape(Rectangle())
                        }
                        .buttonStyle(.plain)
                        .disabled(isPerformingAction)
                        .accessibilityIdentifier("morde.task.action.\(option.id)")
                    }
                }
            }
        }
        if let actionError = store.actionErrors[task.id] {
            MIFTaskInlineError(message: actionError, retry: nil)
        }
    }

    @ViewBuilder
    private func taskSteps(_ task: MordeTask) -> some View {
        let steps = MIFTaskPresentation.visibleSteps(task.steps)
        MIFTaskSection(title: AppLocalized("Steps")) {
            if steps.isEmpty {
                Group {
                    if task.steps == nil {
                        Text(AppLocalized("Step details unavailable"))
                    } else {
                        Text(AppLocalized("No recorded steps"))
                    }
                }
                .font(.subheadline)
                .foregroundStyle(.secondary)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(16)
            } else {
                VStack(alignment: .leading, spacing: 0) {
                    ForEach(Array(steps.enumerated()), id: \.element.id) { index, step in
                        if index > 0 { NanoMuseRowDivider() }
                        HStack(alignment: .top, spacing: 12) {
                            Text("\(step.sequence)")
                                .font(.caption.weight(.semibold).monospacedDigit())
                                .foregroundStyle(ChatColors.secondaryText)
                                .frame(minWidth: 24, alignment: .leading)
                            VStack(alignment: .leading, spacing: 5) {
                                HStack(alignment: .firstTextBaseline, spacing: 8) {
                                    Text(step.title)
                                        .font(.body.weight(.medium))
                                        .fixedSize(horizontal: false, vertical: true)
                                    Spacer(minLength: 4)
                                    Text(MIFTaskPresentation.stepStatusLabel(step.status))
                                        .font(.caption)
                                        .foregroundStyle(.secondary)
                                }
                                if !step.summary.isEmpty {
                                    Text(step.summary)
                                        .font(.subheadline)
                                        .foregroundStyle(.secondary)
                                        .fixedSize(horizontal: false, vertical: true)
                                }
                            }
                        }
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(.horizontal, 16)
                        .padding(.vertical, 13)
                        .accessibilityIdentifier("morde.task.step.\(step.id)")
                    }
                }
            }
        }
    }

    @ViewBuilder
    private func taskActivity(_ task: MordeTask) -> some View {
        let events = MIFTaskPresentation.timelineRows(store.timelineEvents[task.id] ?? [])
        MIFTaskSection(title: AppLocalized("Activity")) {
            VStack(alignment: .leading, spacing: 0) {
                if store.expiredTimelines.contains(task.id) {
                    Text(AppLocalized("Older events expired; showing the current snapshot and available events"))
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(16)
                }
                if let error = store.timelineErrors[task.id] {
                    Text(AppLocalized("Task event stream unavailable: \(error)"))
                        .font(.footnote)
                        .foregroundStyle(.orange)
                        .fixedSize(horizontal: false, vertical: true)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(16)
                }
                if events.isEmpty {
                    if store.emptyTimelines.contains(task.id) {
                        Text(AppLocalized("No event history available"))
                            .font(.subheadline)
                            .foregroundStyle(.secondary)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .padding(16)
                    } else if store.loadingTimelines.contains(task.id) {
                        HStack(spacing: 10) {
                            ProgressView()
                            Text(AppLocalized("Connecting to task events…"))
                                .font(.subheadline)
                                .foregroundStyle(.secondary)
                        }
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(16)
                    } else if store.timelineErrors[task.id] == nil {
                        Text(AppLocalized("Waiting for new task events"))
                            .font(.subheadline)
                            .foregroundStyle(.secondary)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .padding(16)
                    }
                } else {
                    ForEach(Array(events.enumerated()), id: \.element.id) { index, event in
                        if index > 0 || store.expiredTimelines.contains(task.id) { NanoMuseRowDivider() }
                        HStack(alignment: .top, spacing: 12) {
                            Text("\(event.sequence)")
                                .font(.caption.weight(.semibold).monospacedDigit())
                                .foregroundStyle(ChatColors.secondaryText)
                                .frame(minWidth: 24, alignment: .leading)
                            VStack(alignment: .leading, spacing: 4) {
                                Text(event.title)
                                    .font(.body.weight(.medium))
                                    .fixedSize(horizontal: false, vertical: true)
                                if let summary = event.summary, !summary.isEmpty {
                                    Text(summary)
                                        .font(.subheadline)
                                        .foregroundStyle(.secondary)
                                        .fixedSize(horizontal: false, vertical: true)
                                }
                            }
                        }
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(.horizontal, 16)
                        .padding(.vertical, 13)
                        .accessibilityIdentifier("morde.task.event.\(event.sequence)")
                    }
                }
            }
        }
    }

    @ViewBuilder
    private func taskArtifacts(_ task: MordeTask) -> some View {
        MIFTaskSection(title: AppLocalized("Artifacts")) {
            if let artifacts = task.artifacts {
                if artifacts.isEmpty {
                    Text(AppLocalized("No delivered artifacts"))
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(16)
                } else {
                    VStack(alignment: .leading, spacing: 0) {
                        ForEach(Array(artifacts.enumerated()), id: \.element.id) { index, artifact in
                            if index > 0 { NanoMuseRowDivider() }
                            VStack(alignment: .leading, spacing: 4) {
                                Text(artifact.label)
                                    .font(.body.weight(.medium))
                                    .fixedSize(horizontal: false, vertical: true)
                                Text(artifact.media_type)
                                    .font(.caption)
                                    .foregroundStyle(.secondary)
                                    .fixedSize(horizontal: false, vertical: true)
                            }
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .padding(.horizontal, 16)
                            .padding(.vertical, 13)
                            .accessibilityIdentifier("morde.task.artifact.\(artifact.id)")
                        }
                    }
                }
            } else {
                Text(AppLocalized("Artifact details unavailable"))
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(16)
            }
        }
    }

    private func loadDetail(scope: String?) async {
        let generation = UUID()
        detailGeneration = generation
        loadedScopeKey = nil
        detailError = nil
        isLoadingDetail = false
        guard store.isSignedIn, let scope else { return }
        isLoadingDetail = true
        defer {
            if generation == detailGeneration { isLoadingDetail = false }
        }
        do {
            _ = try await store.task(id: taskID)
            guard !Task.isCancelled, generation == detailGeneration, scope == store.scopeKey else { return }
            loadedScopeKey = scope
            isLoadingDetail = false
            await store.watchTimeline(taskID: taskID)
        } catch {
            guard !Task.isCancelled, generation == detailGeneration, scope == store.scopeKey else { return }
            detailError = error.localizedDescription
        }
    }

    private func perform(_ option: MIFTaskActionOption) async {
        guard let scope = loadedScopeKey, scope == store.scopeKey else { return }
        isPerformingAction = true
        defer { isPerformingAction = false }
        _ = await store.actOnTask(taskID: taskID, action: option.action)
    }
}

private struct MIFTaskListRow: View {
    let task: MordeTask

    var body: some View {
        HStack(spacing: 12) {
            Image(systemName: "checklist")
                .font(.system(size: 18, weight: .medium))
                .foregroundStyle(NanoMuseTones.action)
                .frame(width: 26)
            VStack(alignment: .leading, spacing: 7) {
                Text(task.objective)
                    .font(.body.weight(.medium))
                    .lineLimit(2)
                    .multilineTextAlignment(.leading)
                    .frame(maxWidth: .infinity, alignment: .leading)
                HStack(spacing: 8) {
                    Text(task.statusLabel)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    if let currentStep = task.current_step, !currentStep.isEmpty {
                        Text("·")
                            .foregroundStyle(.tertiary)
                        Text(currentStep)
                            .font(.caption)
                            .foregroundStyle(.secondary)
                            .lineLimit(1)
                    }
                }
            }
            Image(systemName: "chevron.right")
                .font(.system(size: 12, weight: .semibold))
                .foregroundStyle(.tertiary)
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 14)
        .contentShape(Rectangle())
    }
}

private struct MIFTaskSection<Content: View>: View {
    let title: String
    @ViewBuilder let content: () -> Content

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text(title)
                .font(.headline)
                .padding(.horizontal, 16)
            NanoMuseCard(content: content)
        }
    }
}

private struct MIFTaskNotice: View {
    let text: String
    let symbol: String

    var body: some View {
        VStack(spacing: 10) {
            Image(systemName: symbol)
                .font(.system(size: 24, weight: .regular))
                .foregroundStyle(.secondary)
            Text(text)
                .font(.subheadline)
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
        }
        .frame(maxWidth: .infinity, minHeight: 150)
        .padding(.horizontal, 24)
    }
}

private struct MIFTaskErrorState: View {
    let message: String
    let retry: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Label(AppLocalized("Unable to load task"), systemImage: "exclamationmark.triangle")
                .font(.headline)
            Text(message)
                .font(.subheadline)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            Button(AppLocalized("Retry loading"), action: retry)
                .font(.subheadline.weight(.medium))
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(16)
        .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 18, style: .continuous))
        .padding(.horizontal, 16)
    }
}

private struct MIFTaskInlineError: View {
    let message: String
    let retry: (() -> Void)?

    var body: some View {
        HStack(alignment: .top, spacing: 10) {
            Text(message)
                .font(.footnote)
                .foregroundStyle(.orange)
                .fixedSize(horizontal: false, vertical: true)
            if let retry {
                Button(AppLocalized("Retry loading"), action: retry)
                    .font(.footnote.weight(.medium))
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(.horizontal, 18)
    }
}
