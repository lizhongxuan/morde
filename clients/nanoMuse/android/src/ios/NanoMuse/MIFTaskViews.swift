import SwiftUI
import UIKit

private struct MIFTaskFileFetchPreview: Identifiable {
    let id = UUID()
    let url: URL
}

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

    static func isFinalEmptyPage(itemCount: Int, didLoadInitial: Bool, nextCursor: String?) -> Bool {
        itemCount == 0 && didLoadInitial && nextCursor == nil
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
    let action: MordeTaskPageAction
}

enum MIFTaskActionRules {
    static func options(for task: MordeTask) -> [MIFTaskActionOption] {
        switch task.status {
        case "queued", "running":
            return [
                MIFTaskActionOption(id: "pause", kind: .pause, title: AppLocalized("Pause"),
                                    action: .pause(expectedStatus: task.status)),
                MIFTaskActionOption(id: "cancel", kind: .cancel, title: AppLocalized("Cancel task"),
                                    action: .cancel(expectedStatus: task.status)),
            ]
        case "paused":
            return [
                MIFTaskActionOption(id: "resume", kind: .resume, title: AppLocalized("Resume"),
                                    action: .resume),
                MIFTaskActionOption(id: "cancel", kind: .cancel, title: AppLocalized("Cancel task"),
                                    action: .cancel(expectedStatus: task.status)),
            ]
        case "waiting_user":
            return [
                MIFTaskActionOption(id: "cancel", kind: .cancel, title: AppLocalized("Cancel task"),
                                    action: .cancel(expectedStatus: task.status)),
            ]
        // nanoMuse: Resolve dynamic labels through catalog format keys, not runtime-built lookup keys.
        case "failed":
            return (task.batch_items ?? []).filter {
                canRetryMordeBatchItem(taskStatus: task.status, itemStatus: $0.status)
            }.map {
                MIFTaskActionOption(id: "retry:\($0.id)", kind: .retryBatchItem,
                                    title: String(format: AppLocalized("Retry batch item: %@"), $0.key),
                                    action: .retryBatchItem(id: $0.id))
            }
        default:
            return []
        }
    }
}

enum MIFTaskPresentation {
    static func headerMood(for task: MordeTask?) -> NanoMuseMood {
        switch task?.status {
        case "queued", "running", "pausing", "cancelling": .working
        case "waiting_user", "paused": .waiting
        case "completed": .happy
        case "failed": .error
        default: .idle
        }
    }

    static func visibleSteps(_ steps: [MordeTask.Step]?, waitingForRevisionStart: Bool = false) -> [MordeTask.Step] {
        (steps ?? []).filter {
            $0.kind != "budget" && !(waitingForRevisionStart && $0.kind == "progress" && $0.title == "开始处理")
        }.sorted { $0.sequence < $1.sequence }
    }

    static func confirmedArtifacts(_ artifacts: [MordeTask.Artifact]?) -> [MordeTask.Artifact] {
        (artifacts ?? []).filter { artifact in
            artifact.verified &&
                artifact.workspace_id?.range(of: "^w[a-z0-9]{7}$", options: .regularExpression) != nil &&
                artifact.workspace_revision.map { $0 >= 0 } == true && artifact.size_bytes.map { $0 >= 0 } == true &&
                artifact.sha256?.range(of: "^[0-9a-f]{64}$", options: .regularExpression) != nil &&
                artifact.path?.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty == false &&
                !artifact.media_type.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
        }
    }

    static func agentGroups(_ task: MordeTask, steps: [MordeTask.Step]) -> [MordeTaskAgentActivityGroup] {
        MordeTaskAgentActivityPolicy.groups(agents: task.agents ?? [], steps: steps.map {
            MordeTaskAgentActivityStep(id: $0.id, sequence: Int64($0.sequence), agentID: $0.agent_id)
        })
    }

    static func agentStatusLabel(_ status: String?) -> String {
        switch status {
        case "running": return AppLocalized("Running")
        case "idle": return AppLocalized("Idle")
        default: return AppLocalized("Status unknown")
        }
    }

    static func stepDetailStateMessage(_ state: String?) -> String {
        switch state {
        case "not_recorded": return AppLocalized("Details were not recorded")
        case "expired": return AppLocalized("Details have expired")
        case "restricted": return AppLocalized("This detail is restricted")
        default: return AppLocalized("Status unknown")
        }
    }

    static func stepMarker(_ step: MordeTask.Step) -> MIFTaskTimelineMarker {
        switch step.status {
        case "succeeded": .completed
        case "cancelled": .unconfirmed
        case "running", "waiting_user": .current
        case "failed": .failed
        case "queued": .upcoming
        default: .unconfirmed
        }
    }

    static func taskListDetail(_ task: MordeTask) -> String? {
        let stepSummary = task.step_count == 0 ? nil :
            String(format: AppLocalized("%1$d steps · %2$d done"),
                   task.step_count, task.completed_step_count)
        let currentStep = task.displayedCurrentStep
        let candidates = task.status == "completed"
            ? [task.result, currentStep]
            : [currentStep, task.result]
        let currentDetail = candidates.compactMap { $0 }.first { !$0.isEmpty }
        let visibleDetails = [stepSummary, currentDetail].compactMap { $0 }
        return visibleDetails.isEmpty ? nil : visibleDetails.joined(separator: " · ")
    }

    static func stepStatusLabel(_ status: String, kind: String? = nil, title: String = "") -> String {
        if MordeTaskStepPresentation.isStartedProgress(kind: kind, title: title, status: status) {
            return AppLocalized("Task started")
        }
        switch status {
        case "queued": return AppLocalized("Queued")
        case "running": return AppLocalized("Running")
        case "waiting_user": return AppLocalized("Waiting for you")
        case "succeeded", "completed": return AppLocalized("Completed")
        case "cancelled": return AppLocalized("Cancelled")
        case "failed": return AppLocalized("Failed")
        case "unknown": return AppLocalized("Status unknown")
        default: return AppLocalized("Status unknown")
        }
    }

    static func canRetryStep(_ step: MordeTask.Step) -> Bool {
        step.retryable == true && step.status == "failed" &&
            MordeTaskResponsePolicy.hasValidStepID(step.id) &&
            MordeTaskResponsePolicy.stepRetryBody(taskRevision: step.task_revision, attempt: step.attempt) != nil
    }

    static func stepFailureExplanation(_ step: MordeTask.Step) -> String? {
        guard step.status == "failed", step.retryable != true else { return nil }
        switch step.failure_reason {
        case "not_dispatched":
            return AppLocalized("Step was not dispatched. Refresh the task and confirm its state.")
        case "tool_failed_after_dispatch":
            return AppLocalized("Step failed after dispatch. Check for partial results before retrying.")
        case "side_effect_unknown":
            return AppLocalized("This step may already have changed something. Check its result before retrying.")
        case "authorization_changed":
            return AppLocalized("Authorization changed. Refresh the task and check authorization before retrying.")
        case "resource_changed":
            return AppLocalized("The target resource changed. Refresh the task and check its state before retrying.")
        default:
            return nil
        }
    }

    static func stepSummary(_ step: MordeTask.Step) -> String {
        let status = stepStatusLabel(step.status, kind: step.kind, title: step.title)
        return [status, MordeTaskStepPresentation.summary(step.summary, title: step.title)]
            .compactMap { $0 }.joined(separator: " · ")
    }

    static func stepDescription(_ step: MordeTask.Step) -> String {
        "\(MordeTaskStepPresentation.title(step.title)) · \(stepSummary(step))"
    }

    static func timelineEmptyState(isLoading: Bool, noHistory: Bool) -> MIFTaskTimelineEmptyState {
        if isLoading { return .loading }
        return noHistory ? .noHistory : .unconfirmed
    }

    static func timelineRows(_ events: [MordeTaskTimelineEvent]) -> [MIFTaskTimelineRow] {
        var seen = Set<Int64>()
        return events.sorted { $0.sequence < $1.sequence }.compactMap { event in
            guard seen.insert(event.sequence).inserted, let display = event.display else { return nil }
            return MIFTaskTimelineRow(id: "event:\(event.sequence)", sequence: event.sequence,
                                      title: display.title, summary: display.summary, status: event.status)
        }
    }

    static func eventChainMarker(eventStatus: String?, taskStatus: String,
                                 index: Int, rowCount: Int) -> MIFTaskTimelineMarker {
        switch eventStatus {
        case "succeeded", "completed": return .completed
        case "failed": return .failed
        case "running", "waiting_user", "pausing", "cancelling": return .current
        case "queued", "pending": return .upcoming
        case .some(_): return .unconfirmed
        case .none: return timelineMarker(taskStatus: taskStatus, index: index, rowCount: rowCount)
        }
    }

    static func timelineMarker(taskStatus: String, index: Int, rowCount: Int) -> MIFTaskTimelineMarker {
        guard rowCount > 0, index == rowCount - 1 else { return .completed }
        return switch taskStatus {
        case "queued", "running", "waiting_user", "pausing", "cancelling": .current
        case "failed": .failed
        case "completed", "cancelled": .completed
        default: .unconfirmed
        }
    }

    static func timelineMarkerLabel(_ marker: MIFTaskTimelineMarker) -> String {
        switch marker {
        case .completed: return AppLocalized("Completed")
        case .current: return AppLocalized("Current")
        case .failed: return AppLocalized("Failed")
        case .upcoming: return AppLocalized("Queued")
        case .unconfirmed: return AppLocalized("Status unknown")
        }
    }
}

enum MIFTaskTimelineEmptyState: Equatable {
    case loading
    case noHistory
    case unconfirmed
}

enum MIFTaskTimelineMarker: Equatable {
    case completed
    case current
    case failed
    case upcoming
    case unconfirmed
}

struct MIFTaskTimelineRow: Identifiable {
    let id: String
    let sequence: Int64
    let title: String
    let summary: String?
    let status: String?
}

private struct MIFTaskTimelineMarkerView: View {
    let marker: MIFTaskTimelineMarker

    var body: some View {
        switch marker {
        case .completed:
            Circle()
                .stroke(Color.green, lineWidth: 2)
                .overlay {
                    Image(systemName: "checkmark")
                        .font(.system(size: 11, weight: .bold))
                        .foregroundStyle(Color.green)
                }
                .frame(width: 24, height: 24)
        case .current:
            Circle()
                .fill(Color.primary)
                .frame(width: 24, height: 24)
                .overlay {
                    Circle()
                        .fill(NanoMuseTones.canvas)
                        .frame(width: 4, height: 4)
                }
        case .failed:
            Circle()
                .stroke(Color.red, lineWidth: 2)
                .overlay {
                    Image(systemName: "xmark")
                        .font(.system(size: 10, weight: .bold))
                        .foregroundStyle(Color.red)
                }
                .frame(width: 24, height: 24)
        case .upcoming:
            Circle()
                .strokeBorder(NanoMuseTones.hairline, lineWidth: 1.5)
                .background(Circle().fill(NanoMuseTones.canvas))
                .frame(width: 24, height: 24)
        case .unconfirmed:
            Circle()
                .strokeBorder(NanoMuseTones.secondaryText.opacity(0.55), lineWidth: 1.5)
                .background(Circle().fill(NanoMuseTones.canvas))
                .frame(width: 24, height: 24)
        }
    }
}

@MainActor
struct MIFTaskEventChainRoom: View {
    var chrome: NanoMuseRoomChrome? = nil
    var onMenu: (() -> Void)? = nil
    var taskID: String? = nil
    @StateObject private var store = MIFTaskPageStore.shared
    @Environment(\.dismiss) private var dismiss
    @State private var loadError: String?
    @State private var isLoadingTask = false
    @State private var loadedScopeKey: String?
    @State private var loadedTaskID: String?
    @State private var loadGeneration = UUID()

    private var selectedTaskID: String? { taskID ?? store.currentTask?.id }

    private var task: MordeTask? {
        guard let id = selectedTaskID, loadedScopeKey == store.scopeKey,
              loadedTaskID == id else { return nil }
        return store.details[id] ?? (store.currentTask?.id == id ? store.currentTask : nil)
    }

    var body: some View {
        VStack(spacing: 0) {
            if let onMenu {
                MordeDesignPageHeader(title: AppLocalized("Event chain"), onMenu: onMenu) { EmptyView() }
            } else if let chrome {
                NanoMuseMenuHeader(title: AppLocalized("Event chain"), onMenu: chrome.onMenu)
            } else {
                NanoMuseTopBar(title: AppLocalized("Event chain"), onBack: { dismiss() })
            }
            ScrollView {
                VStack(alignment: .leading, spacing: 14) {
                    if let task {
                        Text(task.objective)
                            .font(.system(size: 14.5))
                            .foregroundStyle(NanoMuseTones.secondaryText)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .accessibilityIdentifier("morde.eventChain.taskTitle")
                        timeline(taskID: task.id, taskStatus: task.status)
                    } else if !store.isSignedIn {
                        MIFTaskNotice(text: AppLocalized("Sign in to view Morde tasks"), symbol: "person.crop.circle")
                    } else if store.scopeKey == nil {
                        MIFTaskNotice(text: AppLocalized("Confirming the current account scope…"),
                                      symbol: "person.crop.circle")
                    } else if isLoadingTask ||
                                (selectedTaskID != nil && loadedTaskID != selectedTaskID && loadError == nil) {
                        ProgressView(AppLocalized("Loading task details…"))
                            .frame(maxWidth: .infinity, minHeight: 150)
                    } else if let loadError {
                        MIFTaskErrorState(message: loadError)
                    } else if let taskError = selectedTaskID.flatMap({ store.timelineErrors[$0] }) {
                        MIFTaskErrorState(message: taskError)
                    } else {
                        MIFTaskNotice(text: AppLocalized("No current task is available"), symbol: "clock.arrow.circlepath")
                            .accessibilityIdentifier("morde.eventChain.empty")
                    }
                }
                .padding(.horizontal, 22)
                .padding(.top, 2)
                .padding(.bottom, onMenu == nil && chrome != nil ? NanoMuseBottomBar.height : 0)
                .mordeVerticalScrollBounceProbe()
            }
            .background(NanoMuseTones.canvas)
            .refreshable { await reloadCurrentTask() }
            .mordeAlwaysBounceVertically()
        }
        .background(NanoMuseTones.canvas.ignoresSafeArea())
        .toolbar(.hidden, for: .navigationBar)
        .task(id: "\(store.scopeKey ?? "")-\(taskID ?? store.currentTask?.id ?? "")") {
            await reloadCurrentTask()
        }
    }

    @ViewBuilder
    private func timeline(taskID: String, taskStatus: String) -> some View {
        let rows = MIFTaskPresentation.timelineRows(store.timelineEvents[taskID] ?? [])
        if let loadError {
            MIFTaskInlineError(message: loadError)
        }
        if let timelineError = store.timelineErrors[taskID] {
            MIFTaskInlineError(message: timelineError)
                .accessibilityIdentifier("morde.eventChain.timelineError")
        }
        if store.expiredTimelines.contains(taskID) {
            Text(AppLocalized("Older events expired; showing the current snapshot and available events"))
                .font(.footnote)
                .foregroundStyle(NanoMuseTones.secondaryText)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(12)
        }
        if rows.isEmpty {
            if loadError == nil && store.timelineErrors[taskID] == nil {
                switch MIFTaskPresentation.timelineEmptyState(
                    isLoading: store.loadingTimelines.contains(taskID),
                    noHistory: store.emptyTimelines.contains(taskID)) {
                case .loading:
                    ProgressView(AppLocalized("Connecting to task events…"))
                        .frame(maxWidth: .infinity, minHeight: 72)
                        .accessibilityIdentifier("morde.eventChain.timelineLoading")
                case .noHistory:
                    Text(AppLocalized("No event history available"))
                        .foregroundStyle(NanoMuseTones.secondaryText)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .accessibilityIdentifier("morde.eventChain.noHistory")
                case .unconfirmed:
                    MIFTaskInlineError(message: AppLocalized("Task event history has not been confirmed"))
                }
            }
        } else {
            VStack(alignment: .leading, spacing: 0) {
                ForEach(Array(rows.enumerated()), id: \.element.id) { index, event in
                    HStack(alignment: .top, spacing: 16) {
                        let marker = MIFTaskPresentation.eventChainMarker(
                            eventStatus: event.status, taskStatus: taskStatus,
                            index: index, rowCount: rows.count)
                        MIFTaskTimelineMarkerView(marker: marker)
                            .accessibilityElement(children: .ignore)
                            .accessibilityLabel(MIFTaskPresentation.timelineMarkerLabel(marker))
                        VStack(alignment: .leading, spacing: 2) {
                            Text(event.title)
                                .font(.system(size: 15.5, weight: .semibold))
                                .lineSpacing(2.5)
                                .fixedSize(horizontal: false, vertical: true)
                            if let summary = event.summary, !summary.isEmpty {
                                Text(summary)
                                    .font(.system(size: 14))
                                    .lineSpacing(4)
                                    .foregroundStyle(NanoMuseTones.secondaryText)
                                    .fixedSize(horizontal: false, vertical: true)
                            }
                        }
                        .padding(.top, 1)
                    }
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(.bottom, index == rows.count - 1 ? 4 : 20)
                    .background(alignment: .topLeading) {
                        if index < rows.count - 1 {
                            GeometryReader { geometry in
                                Path { path in
                                    path.move(to: CGPoint(x: 11.5, y: 26))
                                    path.addLine(to: CGPoint(x: 11.5, y: geometry.size.height))
                                }
                                .stroke(NanoMuseTones.hairline,
                                        style: StrokeStyle(lineWidth: 1.5, dash: [2, 4]))
                            }
                            .frame(width: 24)
                            .allowsHitTesting(false)
                        }
                    }
                    .accessibilityIdentifier("morde.eventChain.event.\(event.sequence)")
                }
            }
        }
    }

    private func reloadCurrentTask() async {
        let generation = UUID()
        loadGeneration = generation
        let requestedTaskID = selectedTaskID
        let requestedScope = store.scopeKey
        if loadedScopeKey != requestedScope || loadedTaskID != requestedTaskID {
            loadedScopeKey = nil
            loadedTaskID = nil
        }
        loadError = nil
        guard store.isSignedIn, let scope = requestedScope, let taskID = requestedTaskID else {
            isLoadingTask = false
            return
        }
        isLoadingTask = true
        defer {
            if generation == loadGeneration { isLoadingTask = false }
        }
        do {
            _ = try await store.task(id: taskID)
            guard !Task.isCancelled, generation == loadGeneration, scope == store.scopeKey,
                  taskID == selectedTaskID else { return }
            loadedScopeKey = scope
            loadedTaskID = taskID
            isLoadingTask = false
            if !store.loadingTimelines.contains(taskID) {
                await store.watchTimeline(taskID: taskID)
            }
            guard !Task.isCancelled, generation == loadGeneration, scope == store.scopeKey,
                  taskID == selectedTaskID else { return }
        } catch {
            guard !Task.isCancelled, generation == loadGeneration, scope == store.scopeKey,
                  taskID == selectedTaskID else { return }
            loadError = error.localizedDescription
        }
    }
}

@MainActor
struct MIFTaskListRoom: View {
    // nanoMuse shell uses its room header; standalone Morde injects the design header.
    var chrome: NanoMuseRoomChrome? = nil
    var onMenu: (() -> Void)? = nil
    var onNewConversation: (() -> Void)? = nil
    @StateObject private var store = MIFTaskPageStore.shared
    @Environment(\.dismiss) private var dismiss
    @State private var tasks: [MordeTask] = []
    @State private var nextCursor: String?
    @State private var pageError: String?
    @State private var isLoadingInitial = false
    @State private var isLoadingMore = false
    @State private var didLoadInitial = false
    @State private var requestGeneration = UUID()
    @State private var isRefreshing = false
    private var visibleTasks: [MordeTask] {
        tasks.filter { !store.inaccessibleTaskIDs.contains($0.id) }
    }

    var body: some View {
        VStack(spacing: 0) {
            if let onMenu {
                MordeDesignPageHeader(title: AppLocalized("Tasks"), onMenu: onMenu) {
                    Button(action: { onNewConversation?() }) {
                        Text("＋")
                            .font(.system(size: 19, weight: .regular))
                            .foregroundStyle(Color.primary)
                            .frame(width: 38, height: 38)
                            .background(NanoMuseTones.surface, in: Circle())
                            .overlay(Circle().strokeBorder(NanoMuseTones.hairline))
                            .contentShape(Circle())
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel(AppLocalized("New task"))
                    .accessibilityIdentifier("morde.tasks.newTask")
                    .disabled(onNewConversation == nil)
                }
            } else if let chrome {
                NanoMuseMenuHeader(title: AppLocalized("Tasks"), onMenu: chrome.onMenu) {
                    Button {
                        chrome.open("newTask")
                    } label: {
                        Image(systemName: "plus")
                            .font(.system(size: 16, weight: .medium))
                            .foregroundStyle(Color.primary)
                            .frame(width: 38, height: 38)
                            .background(NanoMuseTones.surface, in: Circle())
                            .overlay(Circle().strokeBorder(NanoMuseTones.hairline))
                            .contentShape(Circle())
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel(AppLocalized("New task"))
                    .accessibilityIdentifier("morde.tasks.newTask")
                }
            } else {
                NanoMuseTopBar(title: AppLocalized("Tasks"), onBack: { dismiss() })
            }

            ScrollView {
                LazyVStack(spacing: 0) {
                    listContent
                    if let nextCursor {
                        paginationFooter(for: nextCursor)
                    }
                }
                .padding(.horizontal, 22)
                .padding(.top, 2)
                .padding(.bottom, onMenu != nil ? 108 : (chrome != nil ? NanoMuseBottomBar.height : 0))
                .mordeVerticalScrollBounceProbe()
            }
            .background(NanoMuseTones.canvas)
            .refreshable { await refreshCurrentScope() }
            .simultaneousGesture(
                // Short error lists have no scrollable content to reliably trigger .refreshable.
                DragGesture(minimumDistance: 20).onEnded { value in
                    guard visibleTasks.isEmpty, pageError != nil,
                          value.startLocation.y < 80, value.translation.height > 100 else { return }
                    Task { await refreshCurrentScope() }
                })
            .mordeAlwaysBounceVertically()
            .accessibilityIdentifier("morde.tasks.list")
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
        } else if visibleTasks.isEmpty && isLoadingInitial {
            ProgressView(AppLocalized("Loading tasks…"))
                .frame(maxWidth: .infinity, minHeight: 150)
        } else if visibleTasks.isEmpty, let pageError, !isLoadingMore {
            MIFTaskInlineError(message: pageError)
                .accessibilityIdentifier("morde.tasks.loadError")
        } else if visibleTasks.isEmpty && !tasks.isEmpty {
            MIFTaskNotice(text: AppLocalized("Current account can no longer read this task"), symbol: "lock")
        } else if MIFTaskPageRules.isFinalEmptyPage(
            itemCount: visibleTasks.count, didLoadInitial: didLoadInitial, nextCursor: nextCursor) {
            MIFTaskNotice(text: AppLocalized("No tasks yet"), symbol: "checklist")
                .accessibilityIdentifier("morde.tasks.empty")
        } else if visibleTasks.isEmpty && nextCursor != nil {
            EmptyView()
        } else if visibleTasks.isEmpty {
            MIFTaskNotice(text: AppLocalized("Task list has not loaded"), symbol: "checklist")
        } else {
            if let pageError {
                MIFTaskInlineError(message: pageError)
                    .accessibilityIdentifier("morde.tasks.loadError")
            }
            ForEach(visibleTasks) { task in
                    Group {
                        if onMenu != nil {
                            NavigationLink(value: MordeDesignRoute.task(task.id)) {
                                MIFTaskListRow(task: store.details[task.id] ?? task)
                            }
                        } else {
                            NavigationLink {
                                MIFTaskDetailView(taskID: task.id)
                            } label: {
                                MIFTaskListRow(task: store.details[task.id] ?? task)
                            }
                        }
                }
                    .buttonStyle(.plain)
                    .accessibilityIdentifier("morde.tasks.row.\(task.id)")
            }
        }
    }

    private func paginationFooter(for cursor: String) -> some View {
        Group {
            if isLoadingMore {
                ProgressView()
                    .frame(maxWidth: .infinity, minHeight: 32)
                    .accessibilityIdentifier("morde.tasks.loadingMore")
            } else {
                Color.clear.frame(height: 1)
            }
        }
        .frame(maxWidth: .infinity)
        .id(cursor)
        .onAppear {
            guard !isLoadingInitial, !isLoadingMore, pageError == nil else { return }
            Task { await loadMoreTasks() }
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

    private func refreshCurrentScope() async {
        guard !isRefreshing, store.isSignedIn, let scope = store.scopeKey else { return }
        isRefreshing = true
        defer { isRefreshing = false }
        let generation = UUID()
        requestGeneration = generation
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
            if generation == requestGeneration {
                isLoadingInitial = false
                isLoadingMore = false
            }
            tasks = MIFTaskPageRules.merge(current: appending ? tasks : [], incoming: page.items)
            nextCursor = page.next_cursor
            pageError = nil
            didLoadInitial = true
        } catch {
            guard !Task.isCancelled, generation == requestGeneration, scope == store.scopeKey else { return }
            let requestError = error as NSError
            if requestError.domain == "Morde" && requestError.code == 403 {
                tasks = []
                nextCursor = nil
                didLoadInitial = false
            }
            pageError = error.localizedDescription
        }
    }
}

typealias MIFTaskRoom = MIFTaskListRoom

extension View {
    private var mordeForcesLegacyScrollBounceFixture: Bool {
#if DEBUG
        ProcessInfo.processInfo.arguments.contains("-morde.forceLegacyScrollBounceFixture")
#else
        false
#endif
    }

    @ViewBuilder
    func mordeAlwaysBounceVertically() -> some View {
        if #available(iOS 16.4, *), !mordeForcesLegacyScrollBounceFixture {
            scrollBounceBehavior(.always, axes: .vertical)
        } else {
            self
        }
    }

    @ViewBuilder
    func mordeVerticalScrollBounceProbe() -> some View {
        if #available(iOS 16.4, *), !mordeForcesLegacyScrollBounceFixture {
            self
        } else {
            background(MordeVerticalScrollBounceProbe())
        }
    }
}

private struct MordeVerticalScrollBounceProbe: UIViewRepresentable {
    func makeUIView(context: Context) -> MordeVerticalScrollBounceView { MordeVerticalScrollBounceView() }

    func updateUIView(_ view: MordeVerticalScrollBounceView, context: Context) {
        view.enableVerticalBounce()
    }
}

private final class MordeVerticalScrollBounceView: UIView {
    override func didMoveToSuperview() {
        super.didMoveToSuperview()
        enableVerticalBounce()
    }

    override func didMoveToWindow() {
        super.didMoveToWindow()
        DispatchQueue.main.async { [weak self] in self?.enableVerticalBounce() }
    }

    override func layoutSubviews() {
        super.layoutSubviews()
        enableVerticalBounce()
    }

    func enableVerticalBounce() {
        var ancestor = superview
        while let view = ancestor {
            if let scrollView = view as? UIScrollView {
                scrollView.alwaysBounceVertical = true
                return
            }
            ancestor = view.superview
        }
    }
}

@MainActor
private struct MIFTaskStepActivityRow: View {
    let taskID: String
    let eventSequence: Int64
    let step: MordeTask.Step
    let marker: MIFTaskTimelineMarker
    let showsConnector: Bool
    let groupIndent: CGFloat

    @StateObject private var store = MIFTaskPageStore.shared
    @State private var isExpanded = false
    @State private var detail: MordeTaskStepDetail?
    @State private var loadedDetailRequestKey: String?
    @State private var attemptedRequestKey: String?
    @State private var loadError: String?
    @State private var isLoading = false
    @State private var isRetrying = false

    private var visibleDetail: MordeTaskStepDetail? {
        loadedDetailRequestKey == detailRequestKey ? detail : nil
    }

    private var visibleError: String? {
        loadedDetailRequestKey == detailRequestKey ? loadError : nil
    }

    private var requestKey: String {
        "\(store.scopeKey ?? ""):\(taskID):\(step.id):\(eventSequence):\(isExpanded)"
    }

    private var detailRequestKey: String {
        "\(store.scopeKey ?? ""):\(taskID):\(step.id):\(eventSequence)"
    }

    private var canLoadDetail: Bool {
        step.kind == "tool" || step.tool_name != nil
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(alignment: .top, spacing: 16) {
                MIFTaskTimelineMarkerView(marker: marker)
                    .accessibilityElement(children: .ignore)
                    .accessibilityLabel(MIFTaskPresentation.timelineMarkerLabel(marker))
                VStack(alignment: .leading, spacing: 2) {
                    Text(MordeTaskStepPresentation.title(step.title))
                        .font(.system(size: 15.5, weight: .semibold))
                        .lineSpacing(2.5)
                        .fixedSize(horizontal: false, vertical: true)
                    Text(MIFTaskPresentation.stepSummary(step))
                        .font(.system(size: 14))
                        .lineSpacing(4)
                        .foregroundStyle(NanoMuseTones.secondaryText)
                        .fixedSize(horizontal: false, vertical: true)
                    if let time = MordeTaskResponsePolicy.stepTimeLabel(step.created_at) {
                        Text(verbatim: "\(AppLocalized("Time")) · \(time)")
                            .font(.caption)
                            .foregroundStyle(NanoMuseTones.secondaryText)
                    }
                }
                .padding(.top, 1)
            }

            if canLoadDetail {
                DisclosureGroup(isExpanded: $isExpanded) {
                    detailContent
                        .padding(.leading, 26)
                        .padding(.top, 6)
                } label: {
                    Label(AppLocalized("Details"), systemImage: "doc.text.magnifyingglass")
                        .font(.footnote.weight(.medium))
                        .foregroundStyle(NanoMuseTones.secondaryText)
                }
                .accessibilityIdentifier("morde.task.step.details.\(step.id)")
            }

            if let explanation = MIFTaskPresentation.stepFailureExplanation(step) {
                Text(verbatim: explanation)
                    .font(.footnote)
                    .foregroundStyle(NanoMuseTones.secondaryText)
                    .fixedSize(horizontal: false, vertical: true)
                    .accessibilityIdentifier("morde.task.step.failureReason.\(step.id)")
            }

            if MIFTaskPresentation.canRetryStep(step) {
                Button {
                    guard !isRetrying, !store.actionInFlight else { return }
                    isRetrying = true
                    Task {
                        _ = await store.retryTaskStep(taskID: taskID, stepID: step.id)
                        isRetrying = false
                    }
                } label: {
                    HStack(spacing: 8) {
                        if isRetrying { ProgressView().controlSize(.small) }
                        Text(AppLocalized("Retry step"))
                    }
                }
                .disabled(isRetrying || store.actionInFlight)
                .accessibilityIdentifier("morde.task.step.retry.\(step.id)")
                if let error = store.stepRetryError(taskID: taskID, stepID: step.id) {
                    MIFTaskInlineError(message: error)
                        .accessibilityIdentifier("morde.task.step.retry.error.\(step.id)")
                }
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(.leading, 22 + groupIndent)
        .padding(.bottom, 16)
        .background(alignment: .topLeading) {
            if showsConnector {
                GeometryReader { geometry in
                    Path { path in
                        path.move(to: CGPoint(x: 11.5, y: 26))
                        path.addLine(to: CGPoint(x: 11.5, y: geometry.size.height))
                    }
                    .stroke(NanoMuseTones.secondaryText.opacity(0.3),
                            style: StrokeStyle(lineWidth: 2, dash: [2, 4]))
                }
                .frame(width: 24)
                .padding(.leading, 22 + groupIndent)
                .allowsHitTesting(false)
            }
        }
        .accessibilityElement(children: .contain)
        .accessibilityIdentifier("morde.task.step.\(step.id)")
        .task(id: requestKey) {
            guard isExpanded, let scopeKey = store.scopeKey,
                  attemptedRequestKey != detailRequestKey, visibleDetail == nil else { return }
            if ["not_recorded", "expired", "restricted"].contains(step.detail_state ?? "") { return }
            await loadDetail(scopeKey: scopeKey)
        }
    }

    @ViewBuilder
    private var detailContent: some View {
        if let visibleDetail {
            if visibleDetail.detail_state == "available" {
                VStack(alignment: .leading, spacing: 10) {
                    detailField("Tool", value: MordeTaskStepPresentation.detailToolName(title: step.title,
                        storedToolName: step.tool_name, detailToolName: visibleDetail.tool_name))
                    if let object = MordeTaskStepPresentation.detailObject(title: step.title, object: visibleDetail.object), !object.isEmpty {
                        detailField("Object", value: object)
                    }
                    if let input = visibleDetail.input, !input.isEmpty { detailField("Input", value: input) }
                    if let result = visibleDetail.result, !result.isEmpty { detailField("Result", value: result) }
                    if visibleDetail.truncated || step.truncated == true {
                        Text(AppLocalized("Details are truncated"))
                            .font(.footnote)
                            .foregroundStyle(NanoMuseTones.secondaryText)
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
            } else {
                Text(MIFTaskPresentation.stepDetailStateMessage(visibleDetail.detail_state))
                    .font(.footnote)
                    .foregroundStyle(NanoMuseTones.secondaryText)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
        } else if let visibleError {
            VStack(alignment: .leading, spacing: 8) {
                Text(visibleError)
                    .font(.footnote)
                    .foregroundStyle(NanoMuseTones.secondaryText)
                Button(AppLocalized("Retry")) {
                    guard let scopeKey = store.scopeKey else { return }
                    attemptedRequestKey = nil
                    Task { await loadDetail(scopeKey: scopeKey) }
                }
                .font(.footnote.weight(.semibold))
            }
        } else if isLoading {
            ProgressView {
                Text(verbatim: AppLocalized("Loading details…"))
            }
                .font(.footnote)
        } else if let state = step.detail_state, state != "available" {
            Text(MIFTaskPresentation.stepDetailStateMessage(state))
                .font(.footnote)
                .foregroundStyle(NanoMuseTones.secondaryText)
        }
    }

    private func detailField(_ label: String, value: String) -> some View {
        VStack(alignment: .leading, spacing: 3) {
            Text(verbatim: localizedDetailLabel(label))
                .font(.caption.weight(.semibold))
                .foregroundStyle(NanoMuseTones.secondaryText)
            Text(verbatim: value)
                .font(.footnote.monospaced())
                .textSelection(.enabled)
                .fixedSize(horizontal: false, vertical: true)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func localizedDetailLabel(_ label: String) -> String {
        switch label {
        case "Tool": return AppLocalized("Tool")
        case "Object": return AppLocalized("Object")
        case "Input": return AppLocalized("Input")
        case "Result": return AppLocalized("Result")
        default: return label
        }
    }

    private func loadDetail(scopeKey: String) async {
        guard store.scopeKey == scopeKey else { return }
        let requestContext = detailRequestKey
        isLoading = true
        defer { if store.scopeKey == scopeKey && detailRequestKey == requestContext { isLoading = false } }
        do {
            let response = try await store.taskStepDetail(taskID: taskID, stepID: step.id,
                minimumEventSequence: eventSequence, expectedScopeKey: scopeKey)
            guard !Task.isCancelled, store.scopeKey == scopeKey, detailRequestKey == requestContext else { return }
            detail = response
            loadError = nil
            loadedDetailRequestKey = requestContext
            attemptedRequestKey = requestContext
        } catch is CancellationError {
            return
        } catch {
            guard !Task.isCancelled, store.scopeKey == scopeKey, detailRequestKey == requestContext else { return }
            loadError = AppLocalized("Task details are unavailable")
            loadedDetailRequestKey = requestContext
            attemptedRequestKey = requestContext
        }
    }
}

private struct MIFTaskAgentGroupHeader: View {
    let group: MordeTaskAgentActivityGroup

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            titleView
            Spacer(minLength: 4)
            statusView
        }
        .padding(.leading, CGFloat(group.depth) * 18 + 22)
        .padding(.top, 12)
        .padding(.bottom, 12)
        .accessibilityElement(children: .combine)
        .accessibilityIdentifier("morde.task.agent.\(group.id)")
    }

    private var title: String {
        group.id == "unassigned" ? AppLocalized("Attribution unconfirmed") : group.title
    }

    private var titleView: some View {
        Text(verbatim: title)
            .font(.system(size: 13, weight: .bold, design: .monospaced))
            .foregroundColor(.primary)
    }

    @ViewBuilder
    private var statusView: some View {
        if let status = group.status {
            Text(verbatim: MIFTaskPresentation.agentStatusLabel(status))
                .font(.caption.weight(.medium))
                .foregroundColor(.secondary)
        }
    }
}

@MainActor
struct MIFTaskDetailView: View {
    let taskID: String
    var scrollToActivity = false
    @StateObject private var store = MIFTaskPageStore.shared
    @Environment(\.dismiss) private var dismiss
    @State private var loadedScopeKey: String?
    @State private var requestedScopeKey: String?
    @State private var requestedTaskID: String?
    @State private var detailError: String?
    @State private var isLoadingDetail = false
    @State private var isPerformingAction = false
    @State private var confirmsCancel = false
    @State private var detailGeneration = UUID()
    @State private var actionGeneration = UUID()
    @State private var clarificationAnswer = ""
    @State private var fileFetchPreview: MIFTaskFileFetchPreview?
    @State private var fileFetchToCancel: MordeTaskFileFetch?
    @State private var fileFetchToDelete: MordeTaskFileFetch?
    @State private var fileFetchActionError: String?
    @State private var isPerformingFileFetchAction = false
    @FocusState private var isClarificationFocused: Bool

    private var taskSecondaryText: Color {
        Color(uiColor: UIColor { traits in
            if traits.userInterfaceStyle == .dark {
                return UIColor(red: 0.73, green: 0.73, blue: 0.77, alpha: 1)
            }
            return UIColor(red: 115.0 / 255, green: 115.0 / 255, blue: 120.0 / 255, alpha: 1)
        })
    }

    private var taskStepConnector: Color {
        Color(uiColor: UIColor { traits in
            if traits.userInterfaceStyle == .dark {
                return UIColor(red: 0.227, green: 0.227, blue: 0.235, alpha: 1)
            }
            return UIColor(red: 217.0 / 255, green: 217.0 / 255, blue: 222.0 / 255, alpha: 1)
        })
    }

    private var task: MordeTask? {
        guard loadedScopeKey == store.scopeKey else { return nil }
        return store.details[taskID]
    }

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 10) {
                NanoMuseRoundButton(symbol: "chevron.left", label: AppLocalized("Back"), size: 38) {
                    dismiss()
                }
                .accessibilityIdentifier("morde.task.back")
                Text(task?.objective ?? AppLocalized("Task"))
                    .font(.headline.weight(.semibold))
                    .tracking(-0.17)
                    .lineLimit(1)
                    .accessibilityIdentifier("morde.task.objective")
                Spacer(minLength: 0)
            }
            .padding(.horizontal, 22)
            .frame(minHeight: 52)
            .background(NanoMuseTones.canvas)
            ScrollViewReader { proxy in
                ScrollView {
                    VStack(spacing: 14) {
                        detailContent
                    }
                    .padding(.top, 2)
                    .padding(.bottom, 14)
                    .nmOnChange(of: task?.id) { _ in
                        guard scrollToActivity, task != nil else { return }
                        DispatchQueue.main.async {
                            withAnimation(.easeInOut(duration: 0.2)) {
                                proxy.scrollTo("morde.task.activity.\(taskID)", anchor: .top)
                            }
                        }
                    }
                }
                .background(NanoMuseTones.canvas)
                .accessibilityIdentifier("morde.task.detailScroll")
            }
        }
        .safeAreaInset(edge: .bottom, spacing: 0) {
            if hasActionFooter, let task {
                taskActions(task)
                    .padding(.bottom, 104)
            }
        }
        .background(NanoMuseTones.canvas.ignoresSafeArea())
        .toolbar(.hidden, for: .navigationBar)
        .toolbar {
            ToolbarItemGroup(placement: .keyboard) {
                Spacer()
                Button(AppLocalized("Done")) { isClarificationFocused = false }
                    .accessibilityIdentifier("morde.task.clarification.keyboardDone")
            }
        }
        .confirmationDialog(AppLocalized("Cancel this task?"), isPresented: $confirmsCancel,
                            titleVisibility: .visible) {
            Button(AppLocalized("Cancel task"), role: .destructive) {
                guard let option = actionOptions.first(where: { $0.kind == .cancel }) else { return }
                Task { await perform(option, taskID: taskID) }
            }
            .disabled(isLoadingDetail || detailError != nil || store.actionInFlight)
            Button(AppLocalized("Back"), role: .cancel) {}
        }
        .confirmationDialog(AppLocalized("Cancel this file request?"), isPresented: Binding(
            get: { fileFetchToCancel != nil }, set: { if !$0 { fileFetchToCancel = nil } }),
            titleVisibility: .visible) {
            Button(AppLocalized("Cancel file request"), role: .destructive) {
                guard let fetch = fileFetchToCancel else { return }
                fileFetchToCancel = nil
                Task { await cancelFileFetch(fetch) }
            }
            .accessibilityIdentifier("morde.task.fileFetch.confirmCancel")
            Button(AppLocalized("Back"), role: .cancel) { fileFetchToCancel = nil }
        } message: {
            Text(fileFetchToCancel.map { String(format: AppLocalized("Cancel fetch for %@?"), $0.relative_path) } ?? "")
        }
        .confirmationDialog(AppLocalized("Delete retained file copy?"), isPresented: Binding(
            get: { fileFetchToDelete != nil }, set: { if !$0 { fileFetchToDelete = nil } }),
            titleVisibility: .visible) {
            Button(AppLocalized("Delete cloud copy"), role: .destructive) {
                guard let fetch = fileFetchToDelete else { return }
                fileFetchToDelete = nil
                Task { await deleteFileFetchCopy(fetch) }
            }
            .accessibilityIdentifier("morde.task.fileFetch.confirmDelete")
            Button(AppLocalized("Back"), role: .cancel) { fileFetchToDelete = nil }
        } message: {
            Text(AppLocalized("The PC source file and Task history are unaffected."))
        }
        .sheet(item: $fileFetchPreview) { preview in
            MordeWorkspaceQuickLook(url: preview.url)
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                .accessibilityElement(children: .contain)
                .accessibilityIdentifier("morde.task.fileFetch.preview")
                .onDisappear { removeFileFetchPreview(preview) }
        }
        .onChange(of: store.scopeKey) { _ in
            closeFileFetchPreview()
            fileFetchToCancel = nil
            fileFetchToDelete = nil
            fileFetchActionError = nil
        }
        .task(id: "\(store.scopeKey ?? "")-\(taskID)") {
            await loadDetail(scope: store.scopeKey, taskID: taskID)
        }
        .task(id: "\(store.scopeKey ?? "")-\(taskID)-file-fetches") {
            guard store.isSignedIn, let scope = store.scopeKey else { return }
            while !Task.isCancelled, scope == store.scopeKey {
                do { try await store.refreshFileFetches(taskID: taskID) }
                catch { if Task.isCancelled { return } }
                let taskFinished = ["completed", "failed", "cancelled"].contains(store.details[taskID]?.status ?? "")
                let fetchInFlight = (store.fileFetches[taskID] ?? []).contains {
                    !["completed", "cancelled", "failed"].contains($0.status)
                }
                if taskFinished && !fetchInFlight && store.fileFetchErrors[taskID] == nil { return }
                try? await Task.sleep(nanoseconds: 3_000_000_000)
            }
        }
        .onDisappear { closeFileFetchPreview() }
    }

    @ViewBuilder
    private var detailContent: some View {
        if !store.isSignedIn {
            MIFTaskNotice(text: AppLocalized("Sign in to view Morde tasks"), symbol: "person.crop.circle")
        } else if let task {
            if isLoadingDetail {
                ProgressView(AppLocalized("Refreshing task details…"))
                    .frame(maxWidth: .infinity, minHeight: 40)
            }
            if let detailError {
                MIFTaskInlineError(message: detailError)
            }
            taskSummary(task)
            taskSteps(task)
            taskRelationships(task)
            taskPendingRequest(task)
            taskClarification(task)
            taskActivity(task)
            taskArtifacts(task)
            taskFileFetches(task)
        } else if isLoadingDetail {
            ProgressView(AppLocalized("Loading tasks…"))
                .frame(maxWidth: .infinity, minHeight: 150)
        } else if let detailError {
            MIFTaskErrorState(message: detailError)
        } else {
            MIFTaskNotice(text: AppLocalized("Task details unavailable"), symbol: "exclamationmark.circle")
        }
    }

    private var actionOptions: [MIFTaskActionOption] {
        guard let task else { return [] }
        return MIFTaskActionRules.options(for: task)
    }

    private var hasActionFooter: Bool {
        guard let task else { return false }
        return !actionOptions.isEmpty || store.actionErrors[task.id] != nil
    }

    private func taskSummary(_ task: MordeTask) -> some View {
        let step = task.displayedCurrentStep == task.statusLabel ? nil : task.displayedCurrentStep
        let details = [task.statusLabel, step, task.result]
            .compactMap { $0 }
            .filter { !$0.isEmpty }
        return Text(details.joined(separator: " · "))
            .font(.system(size: 14.5))
            .foregroundStyle(taskSecondaryText)
            .fixedSize(horizontal: false, vertical: true)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.horizontal, 22)
            .padding(.top, 2)
            .accessibilityIdentifier("morde.task.status")
    }

    @ViewBuilder
    private func taskRelationships(_ task: MordeTask) -> some View {
        let dependencies = task.dependencies ?? []
        let childJobs = task.child_jobs ?? []
        if !dependencies.isEmpty || !childJobs.isEmpty || task.result_review != nil {
            MIFTaskSection(title: AppLocalized("Task relationships")) {
                VStack(alignment: .leading, spacing: 14) {
                    if !dependencies.isEmpty {
                        VStack(alignment: .leading, spacing: 8) {
                            Text(AppLocalized("Dependencies"))
                                .font(.subheadline.weight(.semibold))
                            ForEach(dependencies, id: \.task_id) { dependency in
                                HStack(alignment: .top, spacing: 10) {
                                    Image(systemName: dependency.satisfied ? "checkmark.circle.fill" : "clock")
                                        .foregroundStyle(dependency.satisfied ? NanoMuseTones.action : .orange)
                                        .accessibilityHidden(true)
                                    VStack(alignment: .leading, spacing: 3) {
                                        Text(String(format: AppLocalized("%@ · required revision %lld"),
                                                    dependency.task_id, dependency.required_revision))
                                            .font(.subheadline.weight(.medium))
                                        Text(NSLocalizedString(MordeTaskStatusPresentation.localizationKey(
                                            status: dependency.task_status), comment: "Task dependency status"))
                                            .font(.caption)
                                            .foregroundStyle(taskSecondaryText)
                                    }
                                    Spacer(minLength: 4)
                                    Text(AppLocalized(dependency.satisfied ? "Dependency satisfied" : "Waiting for dependency"))
                                        .font(.caption.weight(.medium))
                                        .foregroundStyle(dependency.satisfied ? NanoMuseTones.action : .orange)
                                        .multilineTextAlignment(.trailing)
                                }
                                .accessibilityElement(children: .combine)
                                .accessibilityIdentifier("morde.task.dependency.\(dependency.task_id).\(dependency.required_revision)")
                            }
                        }
                        .accessibilityIdentifier("morde.task.dependencies")
                    }

                    if !childJobs.isEmpty {
                        VStack(alignment: .leading, spacing: 8) {
                            Text(AppLocalized("Child jobs"))
                                .font(.subheadline.weight(.semibold))
                                .accessibilityIdentifier("morde.task.childJobs")
                            ForEach(childJobs, id: \.job_id) { job in
                                HStack(alignment: .top, spacing: 10) {
                                    VStack(alignment: .leading, spacing: 3) {
                                        Text(job.title)
                                            .font(.subheadline.weight(.medium))
                                            .fixedSize(horizontal: false, vertical: true)
                                        Text(String(format: AppLocalized("Revision %lld"), job.task_revision))
                                            .font(.caption)
                                            .foregroundStyle(taskSecondaryText)
                                    }
                                    Spacer(minLength: 4)
                                    Text(MIFTaskPresentation.stepStatusLabel(job.status))
                                        .font(.caption.weight(.medium))
                                        .foregroundStyle(taskSecondaryText)
                                        .multilineTextAlignment(.trailing)
                                }
                                .accessibilityElement(children: .combine)
                                .accessibilityIdentifier("morde.task.childJob.\(job.id)")
                            }
                        }
                    }

                    if let review = task.result_review {
                        VStack(alignment: .leading, spacing: 6) {
                            Text(AppLocalized("Task result review"))
                                .font(.subheadline.weight(.semibold))
                            Text(resultReviewLabel(review.state))
                                .font(.subheadline)
                            Text(String(format: AppLocalized("Source %@ · revision %lld · event %lld"),
                                        review.source_task_id, review.source_task_revision,
                                        review.source_event_sequence))
                                .font(.caption)
                                .foregroundStyle(taskSecondaryText)
                                .fixedSize(horizontal: false, vertical: true)
                        }
                        .accessibilityElement(children: .combine)
                        .accessibilityIdentifier("morde.task.resultReview")
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(16)
            }
        }
    }

    private func resultReviewLabel(_ state: String) -> String {
        switch state {
        case "pending": return AppLocalized("Result review pending")
        case "processing": return AppLocalized("Result review processing")
        case "published": return AppLocalized("Result published")
        case "failed": return AppLocalized("Result review failed")
        default: return AppLocalized("Result review status unknown")
        }
    }

    @ViewBuilder
    private func taskPendingRequest(_ task: MordeTask) -> some View {
        if let pending = task.pending_request {
            MIFTaskSection(title: AppLocalized("Approvals")) {
                VStack(alignment: .leading, spacing: 10) {
                    Text(pending.title)
                        .font(.body.weight(.semibold))
                        .fixedSize(horizontal: false, vertical: true)
                    if let object = pending.object, !object.isEmpty {
                        Text(object)
                            .font(.footnote)
                            .fixedSize(horizontal: false, vertical: true)
                            .accessibilityIdentifier("morde.task.approval.object")
                    }
                    Text(pending.content)
                        .font(.subheadline)
                        .fixedSize(horizontal: false, vertical: true)
                    Text(pending.impact)
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                    Text(AppLocalized("This tool did not provide cost, alternatives, or a revocation method."))
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)

                    if let expiresAt = pending.expirationDate {
                        Text(String(format: AppLocalized("Expires: %@"), expiresAt.formatted(date: .abbreviated, time: .shortened)))
                            .font(.footnote)
                            .foregroundStyle(.secondary)
                            .fixedSize(horizontal: false, vertical: true)
                    }

                    TimelineView(.periodic(from: .now, by: 1)) { timeline in
                        if pending.isExpired {
                            Text(AppLocalized("Approval expired; the requested action was not run."))
                                .font(.footnote.weight(.semibold))
                                .foregroundStyle(.secondary)
                                .accessibilityIdentifier("morde.task.approval.expired")
                        } else if pending.deadlineHasPassed(at: timeline.date) {
                            Text(AppLocalized("Approval deadline passed; refresh to confirm its status."))
                                .font(.footnote)
                                .foregroundStyle(.secondary)
                                .accessibilityIdentifier("morde.task.approval.deadline.\(pending.id)")
                        } else if !pending.canRespond(taskStatus: task.status, at: timeline.date) {
                            Text(AppLocalized("Approval status is unknown; refresh task details before acting."))
                                .font(.footnote)
                                .foregroundStyle(.secondary)
                                .accessibilityIdentifier("morde.task.approval.unknown.\(pending.id)")
                        } else if pending.kind == nil || pending.kind == "approval" {
                            if pending.media_preview_operation_id != nil {
                                store.mediaApprovalControls(task: task, pending: pending)
                                    .disabled(isLoadingDetail || detailError != nil)
                            } else {
                                HStack {
                                    Button(AppLocalized("Reject"), role: .destructive) {
                                        Task { await respondToApproval(pending, taskID: task.id, decision: "reject") }
                                    }
                                    .accessibilityIdentifier("morde.task.approval.reject.\(pending.id)")
                                    Spacer()
                                    Button(AppLocalized("Approve")) {
                                        Task { await respondToApproval(pending, taskID: task.id, decision: "approve") }
                                    }
                                    .buttonStyle(.borderedProminent)
                                    .tint(NanoMuseTones.action)
                                    .accessibilityIdentifier("morde.task.approval.approve.\(pending.id)")
                                }
                                .disabled(store.actionInFlight || isLoadingDetail || detailError != nil ||
                                          !MIFApprovalInboxPolicy.canRespond(pending: pending, taskStatus: task.status, at: timeline.date))
                            }
                        }
                    }

                    if let error = store.actionErrors[task.id] {
                        MIFTaskInlineError(message: error)
                    }
                }
                .padding(16)
            }
        }
    }

    @ViewBuilder
    private func taskClarification(_ task: MordeTask) -> some View {
        if let clarification = task.clarification_request {
            MIFTaskSection(title: AppLocalized("Waiting for your input")) {
                VStack(alignment: .leading, spacing: 10) {
                    Text(clarification.question)
                        .font(.body)
                        .fixedSize(horizontal: false, vertical: true)
                        .accessibilityIdentifier("morde.task.clarification.question")
                    TextField(AppLocalized("回复"), text: $clarificationAnswer, axis: .vertical)
                        .lineLimit(3...6)
                        .textFieldStyle(.roundedBorder)
                        .focused($isClarificationFocused)
                        .disabled(store.actionInFlight || isLoadingDetail || detailError != nil)
                        .accessibilityIdentifier("morde.task.clarification.answer")
                    Button {
                        guard let requestScope = loadedScopeKey,
                              requestScope == store.scopeKey,
                              task.id == self.taskID,
                              !store.actionInFlight, !isLoadingDetail, detailError == nil else { return }
                        let requestGeneration = detailGeneration
                        let requestTaskID = task.id
                        let requestAnswer = clarificationAnswer
                        isClarificationFocused = false
                        Task {
                            let accepted = await store.answerClarification(
                                taskID: requestTaskID, clarificationID: clarification.id,
                                version: clarification.version, answer: requestAnswer)
                            guard !Task.isCancelled, requestGeneration == detailGeneration,
                                  requestScope == store.scopeKey, requestTaskID == self.taskID else { return }
                            if accepted, store.details[requestTaskID]?.clarification_request?.id != clarification.id {
                                clarificationAnswer = ""
                            }
                        }
                    } label: {
                        Label(AppLocalized("Send"), systemImage: "arrow.up")
                    }
                    .buttonStyle(.borderedProminent)
                    .tint(NanoMuseTones.action)
                    .disabled(store.actionInFlight || isLoadingDetail || detailError != nil ||
                              !MordeTaskResponsePolicy.hasValidAnswer(clarificationAnswer))
                    .accessibilityIdentifier("morde.task.clarification.submit")

                    if let error = store.actionErrors[task.id] {
                        MIFTaskInlineError(message: error)
                    }
                }
                .padding(16)
            }
        }
    }

    private func respondToApproval(_ pending: MordeTask.Pending, taskID: String, decision: String) async {
        guard let scope = loadedScopeKey, scope == store.scopeKey, taskID == self.taskID,
              !store.actionInFlight, !isLoadingDetail, detailError == nil,
              let task = store.details[taskID],
              MIFApprovalInboxPolicy.canRespond(pending: pending, taskStatus: task.status) else { return }
        _ = await store.respondToApproval(taskID: taskID, approvalID: pending.id,
                                          version: pending.version, decision: decision)
    }

    @ViewBuilder
    private func taskActions(_ task: MordeTask) -> some View {
        let options = MIFTaskActionRules.options(for: task)
        if !options.isEmpty || store.actionErrors[task.id] != nil {
            VStack(spacing: 8) {
                if let actionError = store.actionErrors[task.id] {
                    MIFTaskInlineError(message: actionError)
                }
                if options.contains(where: { $0.kind == .retryBatchItem }) {
                    VStack(spacing: 8) {
                        ForEach(options) { option in
                            taskActionButton(option, taskID: task.id)
                        }
                    }
                } else if !options.isEmpty {
                    HStack(spacing: 10) {
                        ForEach(options) { option in
                            taskActionButton(option, taskID: task.id)
                        }
                    }
                }
            }
            .padding(.horizontal, 22)
            .padding(.top, 10)
            .padding(.bottom, 10)
            .background(NanoMuseTones.canvas)
        }
    }

    private func taskActionButton(_ option: MIFTaskActionOption, taskID: String) -> some View {
        let actionColor = option.kind == .cancel
            ? Color(red: 0.78, green: 0.12, blue: 0.12)
            : NanoMuseTones.action
        let isDisabled = isPerformingAction || store.actionInFlight || isLoadingDetail || detailError != nil
        return Button {
            if option.kind == .cancel {
                confirmsCancel = true
            } else {
                Task { await perform(option, taskID: taskID) }
            }
        } label: {
            HStack(spacing: 8) {
                if isPerformingAction {
                    ProgressView().controlSize(.small).tint(.white)
                }
                Text(option.title)
                    .lineLimit(1)
                    .minimumScaleFactor(0.85)
            }
            .frame(maxWidth: .infinity)
            .frame(height: 44)
            .font(.system(size: 15.5, weight: .semibold))
            .foregroundStyle(.white)
            .background(actionColor, in: Capsule())
            .contentShape(Capsule())
        }
        .buttonStyle(.plain)
        .disabled(isDisabled)
        .opacity(isDisabled ? 0.55 : 1)
        .accessibilityIdentifier("morde.task.action.\(option.id)")
    }

    @ViewBuilder
    private func taskSteps(_ task: MordeTask) -> some View {
        let steps = MIFTaskPresentation.visibleSteps(task.steps, waitingForRevisionStart: task.isWaitingForRevisionStart)
        let groups = MIFTaskPresentation.agentGroups(task, steps: steps)
        let stepsByID = Dictionary(grouping: steps, by: \.id)
        if steps.isEmpty && groups.isEmpty {
            Text(task.steps == nil ? AppLocalized("Step details unavailable") : AppLocalized("No recorded steps"))
                .font(.subheadline)
                .foregroundStyle(NanoMuseTones.secondaryText)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.horizontal, 22)
                .padding(.vertical, 8)
        } else {
            VStack(alignment: .leading, spacing: 0) {
                ForEach(groups) { group in
                    taskAgentGroup(group, task: task, stepsByID: stepsByID)
                }
            }
        }
    }

    @ViewBuilder
    private func taskAgentGroup(_ group: MordeTaskAgentActivityGroup, task: MordeTask,
                                stepsByID: [String: [MordeTask.Step]]) -> some View {
        VStack(alignment: .leading, spacing: 0) {
            MIFTaskAgentGroupHeader(group: group)
            if group.stepIDs.isEmpty {
                Text(AppLocalized("No recorded steps"))
                    .font(.footnote)
                    .foregroundStyle(NanoMuseTones.secondaryText)
                    .padding(.leading, CGFloat(group.depth) * 18 + 22)
                    .padding(.bottom, 12)
            } else {
                ForEach(Array(group.stepIDs.enumerated()), id: \.offset) { index, stepID in
                    if let step = stepsByID[stepID]?.first {
                        MIFTaskStepActivityRow(taskID: task.id,
                            eventSequence: task.event_sequence ?? 0,
                            step: step,
                            marker: MIFTaskPresentation.stepMarker(step),
                            showsConnector: index < group.stepIDs.count - 1,
                            groupIndent: CGFloat(group.depth) * 18)
                    }
                }
            }
        }
        .accessibilityIdentifier("morde.task.agentGroup.\(group.id)")
    }

    @ViewBuilder
    private func taskActivity(_ task: MordeTask) -> some View {
        let events = MIFTaskPresentation.timelineRows(store.timelineEvents[task.id] ?? [])
        VStack(alignment: .leading, spacing: 0) {
                if store.expiredTimelines.contains(task.id) {
                    Text(AppLocalized("Older events expired; showing the current snapshot and available events"))
                        .font(.footnote)
                        .foregroundStyle(NanoMuseTones.secondaryText)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(.horizontal, 22)
                        .padding(.vertical, 8)
                        .accessibilityIdentifier("morde.task.activity.expired")
                }
                if let error = store.timelineErrors[task.id] {
                    MIFTaskInlineError(
                        message: String(format: AppLocalized("Task event stream unavailable: %@"), error))
                        .accessibilityIdentifier("morde.task.activity.timelineError")
                }
                if events.isEmpty {
                    if store.timelineErrors[task.id] == nil {
                        switch MIFTaskPresentation.timelineEmptyState(
                            isLoading: store.loadingTimelines.contains(task.id),
                            noHistory: store.emptyTimelines.contains(task.id)) {
                        case .noHistory:
                            Text(AppLocalized("No event history available"))
                                .font(.subheadline)
                                .foregroundStyle(NanoMuseTones.secondaryText)
                                .frame(maxWidth: .infinity, alignment: .leading)
                                .padding(.horizontal, 22)
                                .padding(.vertical, 8)
                                .accessibilityIdentifier("morde.task.activity.emptyHistory")
                        case .loading:
                            HStack(spacing: 10) {
                                ProgressView()
                                Text(AppLocalized("Connecting to task events…"))
                                    .font(.subheadline)
                                    .foregroundStyle(NanoMuseTones.secondaryText)
                            }
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .padding(16)
                        case .unconfirmed:
                            MIFTaskInlineError(message: AppLocalized("Task event history has not been confirmed"))
                                .accessibilityIdentifier("morde.task.activity.unconfirmedHistory")
                        }
                    }
                } else {
                    ForEach(Array(events.enumerated()), id: \.element.id) { index, event in
                        let marker = MIFTaskPresentation.eventChainMarker(
                            eventStatus: event.status, taskStatus: task.status,
                            index: index, rowCount: events.count)
                        HStack(alignment: .top, spacing: 16) {
                            MIFTaskTimelineMarkerView(marker: marker)
                            VStack(alignment: .leading, spacing: 2) {
                                Text(event.title)
                                    .font(.system(size: 15.5, weight: .semibold))
                                    .lineSpacing(2.5)
                                    .fixedSize(horizontal: false, vertical: true)
                                if let summary = event.summary, !summary.isEmpty {
                                    Text(summary)
                                        .font(.system(size: 14))
                                        .lineSpacing(4)
                                        .foregroundStyle(taskSecondaryText)
                                        .fixedSize(horizontal: false, vertical: true)
                                }
                            }
                            .padding(.top, 1)
                        }
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(.leading, 22)
                        .padding(.bottom, index == events.count - 1 ? 4 : 20)
                        .background(alignment: .topLeading) {
                            if index < events.count - 1 {
                                GeometryReader { geometry in
                                    Path { path in
                                        path.move(to: CGPoint(x: 11.5, y: 26))
                                        path.addLine(to: CGPoint(x: 11.5, y: geometry.size.height))
                                    }
                                    .stroke(NanoMuseTones.hairline,
                                            style: StrokeStyle(lineWidth: 1.5, dash: [2, 4]))
                                }
                                .frame(width: 24)
                                .padding(.leading, 22)
                                .allowsHitTesting(false)
                            }
                        }
                        .accessibilityElement(children: .combine)
                        .accessibilityLabel("\(event.sequence). \(event.title)")
                        .accessibilityValue(MIFTaskPresentation.timelineMarkerLabel(marker))
                        .accessibilityIdentifier("morde.task.event.\(event.sequence)")
                    }
                }
        }
        .id("morde.task.activity.\(task.id)")
    }

    @ViewBuilder
    private func taskArtifacts(_ task: MordeTask) -> some View {
        MIFTaskSection(title: AppLocalized("Artifacts")) {
            if let artifacts = task.artifacts {
                let confirmed = MIFTaskPresentation.confirmedArtifacts(artifacts)
                if confirmed.isEmpty {
                    Text(AppLocalized("No delivered artifacts"))
                        .font(.subheadline)
                        .foregroundStyle(NanoMuseTones.secondaryText)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(16)
                } else {
                    VStack(alignment: .leading, spacing: 0) {
                        ForEach(Array(confirmed.enumerated()), id: \.element.id) { index, artifact in
                            if index > 0 { NanoMuseRowDivider() }
                            let size = ByteCountFormatter.string(fromByteCount: artifact.size_bytes ?? 0, countStyle: .file)
                            VStack(alignment: .leading, spacing: 4) {
                                Text(artifact.label)
                                    .font(.body.weight(.medium))
                                    .fixedSize(horizontal: false, vertical: true)
                                Text([artifact.path ?? "", artifact.media_type, size,
                                      "v\(artifact.workspace_revision ?? 0)"].filter { !$0.isEmpty }.joined(separator: " · "))
                                    .font(.caption)
                                    .foregroundStyle(NanoMuseTones.secondaryText)
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
                    .foregroundStyle(NanoMuseTones.secondaryText)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(16)
            }
        }
    }

    @ViewBuilder
    private func taskFileFetches(_ task: MordeTask) -> some View {
        if let fetches = store.fileFetches[task.id] {
            MIFTaskSection(title: AppLocalized("PC file requests")) {
                if store.fileFetchErrors[task.id] != nil {
                    MIFTaskInlineError(message: AppLocalized("Unable to load PC file requests."))
                }
                if let fileFetchActionError {
                    MIFTaskInlineError(message: fileFetchActionError)
                        .accessibilityIdentifier("morde.task.fileFetch.actionError")
                        .accessibilityElement(children: .combine)
                }
                if fetches.isEmpty {
                    Text(AppLocalized("No PC file requests"))
                        .font(.subheadline)
                        .foregroundStyle(NanoMuseTones.secondaryText)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(16)
                } else {
                    VStack(alignment: .leading, spacing: 0) {
                        ForEach(Array(fetches.enumerated()), id: \.element.fetch_id) { index, fetch in
                            if index > 0 { NanoMuseRowDivider() }
                            VStack(alignment: .leading, spacing: 7) {
                                Text(String(format: AppLocalized("Source device · %@"), fetch.source_device_name))
                                    .font(.body.weight(.medium))
                                    .fixedSize(horizontal: false, vertical: true)
                                Text(sourceDeviceStatus(fetch))
                                    .font(.subheadline)
                                    .foregroundStyle(taskSecondaryText)
                                Text(String(format: AppLocalized("Authorized directory · %@"), fetch.sourceDirectoryLabel))
                                    .font(.subheadline)
                                    .foregroundStyle(taskSecondaryText)
                                    .accessibilityIdentifier("morde.task.fileFetch.directory.\(fetch.fetch_id)")
                                Text(String(format: AppLocalized("Relative path · %@"), fetch.relative_path))
                                    .font(.subheadline)
                                    .foregroundStyle(taskSecondaryText)
                                    .textSelection(.enabled)
                                    .fixedSize(horizontal: false, vertical: true)
                                    .accessibilityIdentifier("morde.task.fileFetch.path.\(fetch.fetch_id)")
                                if let version = fetch.source_version {
                                    Text(String(format: AppLocalized("Source snapshot %@"), String(version.prefix(12))))
                                        .font(.caption.monospaced())
                                        .foregroundStyle(taskSecondaryText)
                                } else {
                                    Text(AppLocalized("Source version pending"))
                                        .font(.caption)
                                        .foregroundStyle(taskSecondaryText)
                                }
                                Text(taskFileFetchStatus(fetch))
                                    .font(.subheadline.weight(.medium))
                                    .foregroundStyle(fetch.status == "failed" ? .orange : taskSecondaryText)
                                    .fixedSize(horizontal: false, vertical: true)
                                    .accessibilityIdentifier("morde.task.fileFetch.status.\(fetch.fetch_id)")
                                if fetch.status == "failed" {
                                    Text(taskFileFetchFailureReason(fetch))
                                        .font(.caption)
                                        .foregroundStyle(.orange)
                                        .fixedSize(horizontal: false, vertical: true)
                                        .accessibilityIdentifier("morde.task.fileFetch.failureReason.\(fetch.fetch_id)")
                                }
                                if let size = fetch.size_bytes {
                                    Text(String(format: AppLocalized("File size · %@"), Self.fileSize(size)))
                                        .font(.caption)
                                        .foregroundStyle(taskSecondaryText)
                                        .accessibilityIdentifier("morde.task.fileFetch.size.\(fetch.fetch_id)")
                                    Text(String(format: AppLocalized("Transfer progress %@ of %@"),
                                                Self.fileSize(fetch.confirmed_bytes), Self.fileSize(size)))
                                        .font(.caption)
                                        .foregroundStyle(taskSecondaryText)
                                        .accessibilityIdentifier("morde.task.fileFetch.progress.\(fetch.fetch_id)")
                                    ProgressView(value: Double(fetch.confirmed_bytes), total: Double(max(size, 1)))
                                        .accessibilityLabel(AppLocalized("PC file transfer progress"))
                                        .accessibilityIdentifier("morde.task.fileFetch.progressView.\(fetch.fetch_id)")
                                } else {
                                    Text(String(format: AppLocalized("Received so far · %@"), Self.fileSize(fetch.confirmed_bytes)))
                                        .font(.caption)
                                        .foregroundStyle(taskSecondaryText)
                                }
                                if let digest = fetch.source_sha256 {
                                    Text(String(format: AppLocalized("SHA-256 · %@"), digest))
                                        .font(.caption.monospaced())
                                        .foregroundStyle(taskSecondaryText)
                                        .textSelection(.enabled)
                                        .fixedSize(horizontal: false, vertical: true)
                                        .accessibilityIdentifier("morde.task.fileFetch.sha256.\(fetch.fetch_id)")
                                } else {
                                    Text(AppLocalized("File hash pending"))
                                        .font(.caption)
                                        .foregroundStyle(taskSecondaryText)
                                }
                                Text(taskFileFetchParseStatus(fetch))
                                    .font(.caption)
                                    .foregroundStyle(taskSecondaryText)
                                    .fixedSize(horizontal: false, vertical: true)
                                    .accessibilityIdentifier("morde.task.fileFetch.parseStatus.\(fetch.fetch_id)")
                                Text(taskFileFetchCopyStatus(fetch))
                                    .font(.caption)
                                    .foregroundStyle(taskSecondaryText)
                                    .fixedSize(horizontal: false, vertical: true)
                                    .accessibilityIdentifier("morde.task.fileFetch.copyStatus.\(fetch.fetch_id)")
                                HStack(spacing: 10) {
                                    if fetch.canOpen {
                                        Button(AppLocalized("Open file")) { Task { await previewFileFetch(fetch, taskID: task.id) } }
                                            .accessibilityIdentifier("morde.task.fileFetch.open.\(fetch.fetch_id)")
                                    }
                                    if fetch.canCancel {
                                        Button(fetch.status == "cancelling" ? AppLocalized("Cancellation pending") : AppLocalized("Cancel file request")) {
                                            fileFetchToCancel = fetch
                                        }
                                        .disabled(fetch.status == "cancelling" || isPerformingFileFetchAction || store.actionInFlight)
                                        .accessibilityIdentifier("morde.task.fileFetch.cancel.\(fetch.fetch_id)")
                                    }
                                    if fetch.canDeleteCopy {
                                        Button(AppLocalized("Delete cloud copy"), role: .destructive) { fileFetchToDelete = fetch }
                                            .disabled(isPerformingFileFetchAction || store.actionInFlight)
                                            .accessibilityIdentifier("morde.task.fileFetch.delete.\(fetch.fetch_id)")
                                    }
                                }
                                .buttonStyle(.bordered)
                            }
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .padding(.horizontal, 16)
                            .padding(.vertical, 13)
                        }
                    }
                }
            }
        } else if store.fileFetchErrors[task.id] != nil {
            MIFTaskSection(title: AppLocalized("PC file requests")) {
                MIFTaskInlineError(message: AppLocalized("Unable to load PC file requests."))
                    .accessibilityIdentifier("morde.task.fileFetch.error")
            }
        }
    }

    private func taskFileFetchStatus(_ fetch: MordeTaskFileFetch) -> String {
        switch fetch.status {
        case "queued": return AppLocalized("Waiting to request file")
        case "waiting_device": return AppLocalized("Waiting for PC to reconnect")
        case "transferring": return AppLocalized("Receiving file from PC")
        case "verifying": return AppLocalized("Verifying received file")
        case "completed": return AppLocalized("File received")
        case "cancelling": return AppLocalized("Cancellation pending")
        case "cancelled": return AppLocalized("File request cancelled")
        case "failed": return AppLocalized("File request failed")
        default: return AppLocalized("File request status unknown")
        }
    }

    private func taskFileFetchFailureReason(_ fetch: MordeTaskFileFetch) -> String {
        switch fetch.failure_reason {
        case "access_denied": AppLocalized("File access denied or revoked")
        case "source_changed": AppLocalized("The source file changed during transfer")
        case "file_unavailable": AppLocalized("The source file is unavailable")
        case "limit_exceeded": AppLocalized("File size or storage limit exceeded")
        case "unsupported": AppLocalized("This file type is not supported")
        case "source_unavailable": AppLocalized("The source device or service is unavailable")
        case "transfer_failed": AppLocalized("File transfer did not complete")
        default: AppLocalized("A safe failure reason is unavailable")
        }
    }

    private func sourceDeviceStatus(_ fetch: MordeTaskFileFetch) -> String {
        if fetch.status == "waiting_device" || store.sourceDeviceOnline(installationID: fetch.source_installation_id) == false {
            return AppLocalized("Source device offline")
        }
        return store.sourceDeviceOnline(installationID: fetch.source_installation_id) == true
            ? AppLocalized("Source device online")
            : AppLocalized("Device status unavailable")
    }

    private func taskFileFetchParseStatus(_ fetch: MordeTaskFileFetch) -> String {
        switch fetch.parse_status {
        case "extracted": return AppLocalized("Safe text was extracted for the task as untrusted data.")
        case "raw_only": return AppLocalized("The file is stored, but its content was not read by the model.")
        case "failed":
            let reason = fetch.parse_error.map(taskFileFetchParseError) ?? AppLocalized("Content unavailable")
            return String(format: AppLocalized("File transfer completed; content extraction failed: %@"), reason)
        case "unavailable": return AppLocalized("File content is unavailable to the task.")
        default: return AppLocalized("File content is being prepared.")
        }
    }

    private func taskFileFetchParseError(_ error: String) -> String {
        switch error {
        case "invalid_encoding": return AppLocalized("Text encoding is invalid")
        case "invalid_document": return AppLocalized("Document could not be parsed")
        case "result_too_large": return AppLocalized("Extracted content is too large")
        case "content_unavailable": return AppLocalized("Content unavailable")
        case "integrity_failed": return AppLocalized("File integrity check failed")
        default: return AppLocalized("Content unavailable")
        }
    }

    private func taskFileFetchCopyStatus(_ fetch: MordeTaskFileFetch) -> String {
        switch fetch.content_state {
        case "retained":
            guard let expiresAt = fetch.expires_at else { return AppLocalized("Cloud copy retained") }
            return String(format: AppLocalized("Cloud copy retained until %@"), expiresAt)
        case "deleted": return AppLocalized("Cloud copy deleted")
        case "expired": return AppLocalized("Cloud copy expired")
        default: return AppLocalized("No cloud copy available")
        }
    }

    private static func fileSize(_ bytes: Int64) -> String {
        ByteCountFormatter.string(fromByteCount: bytes, countStyle: .file)
    }

    private func previewFileFetch(_ fetch: MordeTaskFileFetch, taskID: String) async {
        let requestedScope = store.scopeKey
        guard fetch.canOpen, let fileExtension = fetch.previewExtension,
              fetch.isValid(for: taskID), let requestedScope,
              requestedScope == loadedScopeKey, !isPerformingFileFetchAction, !store.actionInFlight else { return }
        isPerformingFileFetchAction = true
        defer { isPerformingFileFetchAction = false }
        fileFetchActionError = nil
        do {
            let data = try await store.fileFetchContent(taskID: taskID, fetchID: fetch.fetch_id)
            guard requestedScope == store.scopeKey, requestedScope == loadedScopeKey else { return }
            let url = FileManager.default.temporaryDirectory.appendingPathComponent(
                "morde-task-fetch-\(fetch.fetch_id)-\(UUID().uuidString).\(fileExtension)")
            try data.write(to: url, options: .atomic)
            guard requestedScope == store.scopeKey, requestedScope == loadedScopeKey else {
                try? FileManager.default.removeItem(at: url)
                return
            }
            fileFetchPreview = MIFTaskFileFetchPreview(url: url)
        } catch {
            guard requestedScope == store.scopeKey else { return }
            let failure = error as NSError
            fileFetchActionError = failure.domain == "MordeTaskFileFetchIntegrity"
                ? AppLocalized("The downloaded file failed integrity verification.")
                : AppLocalized("Could not open the file.")
            try? await store.refreshFileFetches(taskID: taskID)
        }
    }

    private func cancelFileFetch(_ fetch: MordeTaskFileFetch) async {
        guard fetch.canCancel, !isPerformingFileFetchAction, !store.actionInFlight else { return }
        isPerformingFileFetchAction = true
        defer { isPerformingFileFetchAction = false }
        fileFetchActionError = nil
        do { try await store.cancelFileFetch(taskID: taskID, fetchID: fetch.fetch_id) }
        catch {
            fileFetchActionError = AppLocalized("Could not cancel the file request.")
            try? await store.refreshFileFetches(taskID: taskID)
        }
    }

    private func deleteFileFetchCopy(_ fetch: MordeTaskFileFetch) async {
        guard fetch.canDeleteCopy, !isPerformingFileFetchAction, !store.actionInFlight else { return }
        isPerformingFileFetchAction = true
        defer { isPerformingFileFetchAction = false }
        fileFetchActionError = nil
        do { try await store.deleteFileFetchCopy(taskID: taskID, fetchID: fetch.fetch_id) }
        catch {
            fileFetchActionError = AppLocalized("Could not delete the cloud copy.")
            try? await store.refreshFileFetches(taskID: taskID)
        }
    }

    private func removeFileFetchPreview(_ preview: MIFTaskFileFetchPreview) {
        try? FileManager.default.removeItem(at: preview.url)
    }

    private func closeFileFetchPreview() {
        guard let preview = fileFetchPreview else { return }
        fileFetchPreview = nil
        removeFileFetchPreview(preview)
    }

    private func loadDetail(scope: String?, taskID: String) async {
        let requestedTaskID = taskID
        let contextChanged = requestedScopeKey != scope || self.requestedTaskID != requestedTaskID
        requestedScopeKey = scope
        self.requestedTaskID = requestedTaskID
        if contextChanged {
            actionGeneration = UUID()
            isPerformingAction = false
            confirmsCancel = false
            clarificationAnswer = ""
            isClarificationFocused = false
        }
        let generation = UUID()
        detailGeneration = generation
        if contextChanged { loadedScopeKey = nil }
        detailError = nil
        isLoadingDetail = false
        guard store.isSignedIn, let scope else { return }
        isLoadingDetail = true
        defer {
            if generation == detailGeneration { isLoadingDetail = false }
        }
        do {
            _ = try await store.task(id: requestedTaskID)
            guard !Task.isCancelled, generation == detailGeneration,
                  scope == store.scopeKey, requestedTaskID == self.taskID else { return }
            loadedScopeKey = scope
            isLoadingDetail = false
            await store.watchTimeline(taskID: requestedTaskID)
            guard !Task.isCancelled, generation == detailGeneration,
                  scope == store.scopeKey, requestedTaskID == self.taskID else { return }
            if store.details[requestedTaskID] == nil,
               let timelineError = store.timelineErrors[requestedTaskID] {
                detailError = timelineError
            }
        } catch {
            guard !Task.isCancelled, generation == detailGeneration,
                  scope == store.scopeKey, requestedTaskID == self.taskID else { return }
            detailError = error.localizedDescription
        }
    }

    private func perform(_ option: MIFTaskActionOption, taskID: String) async {
        guard let scope = loadedScopeKey, scope == store.scopeKey,
              taskID == self.taskID, !store.actionInFlight,
              !isLoadingDetail, detailError == nil else { return }
        let generation = UUID()
        actionGeneration = generation
        isPerformingAction = true
        defer {
            if generation == actionGeneration { isPerformingAction = false }
        }
        _ = await store.actOnTask(taskID: taskID, action: option.action)
    }

}

private struct MIFTaskListRow: View {
    let task: MordeTask

    private var hasListDetail: Bool {
        MIFTaskPresentation.taskListDetail(task) != nil
    }

    private enum IconPalette {
        static let activeStart = Color(red: 183.0 / 255, green: 186.0 / 255, blue: 201.0 / 255)
        static let activeEnd = Color(red: 142.0 / 255, green: 147.0 / 255, blue: 168.0 / 255)
        static let completed = Color(red: 52.0 / 255, green: 199.0 / 255, blue: 89.0 / 255)
        static let failed = Color(red: 200.0 / 255, green: 30.0 / 255, blue: 30.0 / 255)
        static let detailText = Color(red: 115.0 / 255, green: 115.0 / 255, blue: 120.0 / 255)
        static let tertiaryText = Color(red: 172.0 / 255, green: 172.0 / 255, blue: 177.0 / 255)
        static let plainBackground = Color(red: 236.0 / 255, green: 236.0 / 255, blue: 240.0 / 255)
        static let failedBackground = Color(red: 253.0 / 255, green: 236.0 / 255, blue: 236.0 / 255)
    }

    var body: some View {
        HStack(alignment: .top, spacing: 13) {
            statusIcon
            VStack(alignment: .leading, spacing: 0) {
                Text(task.objective)
                    .font(.system(size: 16, weight: .semibold))
                    .tracking(-0.16)
                    .lineSpacing(1.5)
                    .multilineTextAlignment(.leading)
                    .fixedSize(horizontal: false, vertical: true)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(.bottom, hasListDetail ? 2 : 3)
                if let detail = MIFTaskPresentation.taskListDetail(task) {
                    Text(detail)
                        .font(.system(size: 14))
                        .lineSpacing(3.5)
                        .foregroundStyle(IconPalette.detailText)
                        .fixedSize(horizontal: false, vertical: true)
                        .padding(.bottom, 3)
                        .accessibilityIdentifier("morde.tasks.row.detail.\(task.id)")
                }
                Text(task.statusLabel)
                    .font(.system(size: 13))
                    .foregroundStyle(IconPalette.tertiaryText)
                    .accessibilityIdentifier("morde.tasks.row.status.\(task.id)")
            }
            .padding(.top, 1)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(.vertical, 13)
        .frame(minHeight: hasListDetail ? 91.6 : 0, alignment: .topLeading)
        .contentShape(Rectangle())
    }

    @ViewBuilder
    private var statusIcon: some View {
        if task.status == "completed" {
            Canvas { context, size in
                let scale = min(size.width, size.height) / 24
                var checkmark = Path()
                checkmark.move(to: CGPoint(x: 4.5 * scale, y: 12.5 * scale))
                checkmark.addLine(to: CGPoint(x: 10 * scale, y: 18 * scale))
                checkmark.addLine(to: CGPoint(x: 19.5 * scale, y: 6.5 * scale))
                context.stroke(
                    checkmark,
                    with: .color(IconPalette.completed),
                    style: StrokeStyle(lineWidth: 2.4 * scale, lineCap: .round, lineJoin: .round)
                )
            }
            .frame(width: 20, height: 20)
            .frame(width: 44, height: 44)
            .background(IconPalette.plainBackground, in: Circle())
        } else if task.status == "failed" {
            Image(systemName: "xmark")
                .font(.system(size: 18, weight: .semibold))
                .foregroundStyle(IconPalette.failed)
                .frame(width: 44, height: 44)
                .background(IconPalette.failedBackground, in: Circle())
        } else {
            Canvas { context, size in
                let scale = min(size.width, size.height) / 24
                let nodes = [
                    CGPoint(x: 6 * scale, y: 6 * scale),
                    CGPoint(x: 18 * scale, y: 8 * scale),
                    CGPoint(x: 10 * scale, y: 18 * scale),
                ]

                var nodeOutlines = Path()
                for node in nodes {
                    let radius = 2.3 * scale
                    nodeOutlines.addEllipse(in: CGRect(x: node.x - radius, y: node.y - radius,
                                                       width: radius * 2, height: radius * 2))
                }
                context.stroke(nodeOutlines, with: .color(.white), lineWidth: 1.7 * scale)

                var edges = Path()
                edges.move(to: CGPoint(x: 8.2 * scale, y: 6.8 * scale))
                edges.addLine(to: CGPoint(x: 15.7 * scale, y: 7.7 * scale))
                edges.move(to: CGPoint(x: 7 * scale, y: 8.2 * scale))
                edges.addLine(to: CGPoint(x: 8.9 * scale, y: 15.6 * scale))
                edges.move(to: CGPoint(x: 16.7 * scale, y: 9.9 * scale))
                edges.addLine(to: CGPoint(x: 11.5 * scale, y: 15.8 * scale))
                context.stroke(edges, with: .color(.white), lineWidth: 1.7 * scale)
            }
            .frame(width: 22, height: 22)
            .frame(width: 44, height: 44)
            .background(
                LinearGradient(colors: [IconPalette.activeStart, IconPalette.activeEnd],
                               startPoint: .topLeading, endPoint: .bottomTrailing),
                in: Circle()
            )
        }
    }
}

private struct MIFTaskSection<Content: View>: View {
    let title: String
    @ViewBuilder let content: () -> Content

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            if !title.isEmpty {
                Text(title)
                    .font(.headline)
                    .padding(.horizontal, 16)
            }
            content()
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(.horizontal, 6)
    }
}

struct MIFTaskNotice: View {
    let text: String
    let symbol: String

    var body: some View {
        VStack(spacing: 10) {
            Image(systemName: symbol)
                .font(.system(size: 24, weight: .regular))
                .foregroundStyle(NanoMuseTones.secondaryText)
                .accessibilityHidden(true)
            Text(text)
                .font(.subheadline)
                .foregroundStyle(NanoMuseTones.secondaryText)
                .multilineTextAlignment(.center)
                .fixedSize(horizontal: false, vertical: true)
        }
        .frame(maxWidth: .infinity, minHeight: 150)
        .padding(.horizontal, 24)
        .accessibilityElement(children: .combine)
    }
}

struct MIFTaskErrorState: View {
    let message: String

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Label(AppLocalized("Unable to load task"), systemImage: "exclamationmark.triangle")
                .font(.headline)
            Text(message)
                .font(.subheadline)
                .foregroundStyle(NanoMuseTones.secondaryText)
                .fixedSize(horizontal: false, vertical: true)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(16)
        .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 18, style: .continuous))
        .padding(.horizontal, 16)
        .accessibilityElement(children: .combine)
        .accessibilityIdentifier("morde.task.loadError")
    }
}

struct MIFTaskPlanSection: View {
    let task: MordeTask

    var body: some View {
        if let plan = task.plan, plan.task_revision == task.current_revision {
            VStack(alignment: .leading, spacing: 6) {
                Text("\(AppLocalized("Execution plan")) · \(AppLocalized("Revision")) \(plan.revision)")
                    .font(.caption.weight(.semibold))
                Text(plan.goal).font(.caption).fixedSize(horizontal: false, vertical: true)
                Text("\(AppLocalized("Cost estimate")): \(AppLocalized("Unknown"))")
                    .font(.caption2).foregroundStyle(NanoMuseTones.secondaryText)
                ForEach(plan.steps) { step in
                    VStack(alignment: .leading, spacing: 2) {
                        HStack {
                            Text(step.title).font(.caption.weight(.medium))
                            Spacer()
                            Text(planStepStatus(step.status)).font(.caption2).foregroundStyle(NanoMuseTones.secondaryText)
                        }
                        if !step.description.isEmpty { Text(step.description).font(.caption2).foregroundStyle(NanoMuseTones.secondaryText) }
                        if !step.depends_on.isEmpty { Text("\(AppLocalized("Depends on")): \(step.depends_on.joined(separator: ", "))").font(.caption2) }
                        if !step.outputs.isEmpty { Text("\(AppLocalized("Outputs")): \(step.outputs.joined(separator: ", "))").font(.caption2) }
                        if !step.completion_criteria.isEmpty { Text("\(AppLocalized("Completion criteria")): \(step.completion_criteria.joined(separator: "; "))").font(.caption2) }
                    }
                    .fixedSize(horizontal: false, vertical: true)
                }
            }
            .padding(10)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(NanoMuseTones.fill, in: RoundedRectangle(cornerRadius: 12, style: .continuous))
            .accessibilityElement(children: .contain)
            .accessibilityIdentifier("morde.task.plan")
        }
    }

    private func planStepStatus(_ status: String) -> String {
        switch status {
        case "in_progress": AppLocalized("Running")
        case "completed": AppLocalized("Completed")
        case "blocked": AppLocalized("Blocked")
        default: AppLocalized("Queued")
        }
    }
}

private struct MIFTaskInlineError: View {
    let message: String

    var body: some View {
        HStack(alignment: .top, spacing: 10) {
            Text(message)
                .font(.footnote)
                .foregroundStyle(.orange)
                .fixedSize(horizontal: false, vertical: true)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(.horizontal, 18)
        .accessibilityElement(children: .contain)
    }
}
