import SwiftUI
import UIKit

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
        case "unknown": return .unconfirmed
        default: return timelineMarker(taskStatus: taskStatus, index: index, rowCount: rowCount)
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
                    guard tasks.isEmpty, pageError != nil,
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
        } else if tasks.isEmpty && isLoadingInitial {
            ProgressView(AppLocalized("Loading tasks…"))
                .frame(maxWidth: .infinity, minHeight: 150)
        } else if tasks.isEmpty, let pageError, !isLoadingMore {
            MIFTaskInlineError(message: pageError)
                .accessibilityIdentifier("morde.tasks.loadError")
        } else if MIFTaskPageRules.isFinalEmptyPage(
            itemCount: tasks.count, didLoadInitial: didLoadInitial, nextCursor: nextCursor) {
            MIFTaskNotice(text: AppLocalized("No tasks yet"), symbol: "checklist")
                .accessibilityIdentifier("morde.tasks.empty")
        } else if tasks.isEmpty && nextCursor != nil {
            EmptyView()
        } else if tasks.isEmpty {
            MIFTaskNotice(text: AppLocalized("Task list has not loaded"), symbol: "checklist")
        } else {
            if let pageError {
                MIFTaskInlineError(message: pageError)
                    .accessibilityIdentifier("morde.tasks.loadError")
            }
            ForEach(tasks) { task in
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
                    .padding(.bottom, hasActionFooter ? 170 : 14)
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
            }
        }
        .overlay(alignment: .bottom) {
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
        .task(id: "\(store.scopeKey ?? "")-\(taskID)") {
            await loadDetail(scope: store.scopeKey, taskID: taskID)
        }
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
            taskPendingRequest(task)
            taskClarification(task)
            taskActivity(task)
            taskArtifacts(task)
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
    private func taskPendingRequest(_ task: MordeTask) -> some View {
        if let pending = task.pending_request {
            MIFTaskSection(title: AppLocalized("Approvals")) {
                VStack(alignment: .leading, spacing: 10) {
                    Text(pending.title)
                        .font(.body.weight(.semibold))
                        .fixedSize(horizontal: false, vertical: true)
                    Text(pending.content)
                        .font(.subheadline)
                        .fixedSize(horizontal: false, vertical: true)
                    Text(pending.impact)
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)

                    if pending.kind == nil || pending.kind == "approval" {
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
                            .disabled(store.actionInFlight || isLoadingDetail || detailError != nil)
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
              !store.actionInFlight, !isLoadingDetail, detailError == nil else { return }
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
        if steps.isEmpty {
            Text(task.steps == nil ? AppLocalized("Step details unavailable") : AppLocalized("No recorded steps"))
                .font(.subheadline)
                .foregroundStyle(NanoMuseTones.secondaryText)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.horizontal, 22)
                .padding(.vertical, 8)
        } else {
            VStack(alignment: .leading, spacing: 0) {
                ForEach(Array(steps.enumerated()), id: \.element.id) { index, step in
                    HStack(alignment: .top, spacing: 16) {
                        taskStepMarker(step)
                        VStack(alignment: .leading, spacing: 2) {
                            Text(MordeTaskStepPresentation.title(step.title))
                                .font(.system(size: 15.5, weight: .semibold))
                                .lineSpacing(2.5)
                                .fixedSize(horizontal: false, vertical: true)
                            Text(MIFTaskPresentation.stepSummary(step))
                                .font(.system(size: 14))
                                .lineSpacing(4)
                                .foregroundStyle(taskSecondaryText)
                                .fixedSize(horizontal: false, vertical: true)
                        }
                        .padding(.top, 1)
                    }
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(.leading, 22)
                    .padding(.bottom, index == steps.count - 1 ? 4 : 20)
                    .background(alignment: .topLeading) {
                        if index < steps.count - 1 {
                            GeometryReader { geometry in
                                Path { path in
                                    path.move(to: CGPoint(x: 11.5, y: 26))
                                    path.addLine(to: CGPoint(x: 11.5, y: geometry.size.height))
                                }
                                .stroke(taskStepConnector,
                                        style: StrokeStyle(lineWidth: 2, dash: [2, 4]))
                            }
                            .frame(width: 24)
                            .padding(.leading, 22)
                            .allowsHitTesting(false)
                        }
                    }
                    .accessibilityElement(children: .combine)
                    .accessibilityValue(MIFTaskPresentation.stepStatusLabel(step.status, kind: step.kind, title: step.title))
                    .accessibilityIdentifier("morde.task.step.\(step.id)")
                }
            }
        }
    }

    @ViewBuilder
    private func taskStepMarker(_ step: MordeTask.Step) -> some View {
        let marker: MIFTaskTimelineMarker = switch step.status {
        case "succeeded": .completed
        case "cancelled": .unconfirmed
        case "running", "waiting_user": .current
        case "failed": .failed
        case "queued": .upcoming
        default: .unconfirmed
        }
        MIFTaskTimelineMarkerView(marker: marker)
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
                if artifacts.isEmpty {
                    Text(AppLocalized("No delivered artifacts"))
                        .font(.subheadline)
                        .foregroundStyle(NanoMuseTones.secondaryText)
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
        .accessibilityElement(children: .contain)
        .accessibilityIdentifier("morde.task.loadError")
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
