import SwiftUI

enum MIFWorkItemPresentation {
    static func stateLabel(_ state: String) -> String {
        switch state {
        case "open": return "开放"
        case "blocked": return "已阻塞"
        case "delivered": return "已交付"
        default: return "状态未知"
        }
    }

    static func canManage(role: String?) -> Bool {
        MordeProjectPolicy.canManageWorkItems(role: role)
    }

    static func canBlock(item: MordeProjectWorkItem, canManage: Bool) -> Bool {
        canManage && item.state == "open"
    }

    static func canResume(item: MordeProjectWorkItem, canManage: Bool) -> Bool {
        canManage && item.state == "blocked"
    }

    static func canLinkTask(item: MordeProjectWorkItem, taskID: String, role: String?) -> Bool {
        MordeProjectPolicy.canLinkTask(role: role, state: item.state, taskID: taskID, taskIDs: item.task_ids)
    }

    static func canRecordDelivery(item: MordeProjectWorkItem, taskID: String,
                                  target: MIFWorkItemDeliveryTarget, role: String?) -> Bool {
        MordeProjectPolicy.canRecordDelivery(role: role, state: item.state, taskID: taskID,
                                             taskIDs: item.task_ids, workspaceID: target.workspaceID,
                                             revision: target.revision)
    }
}

private struct MIFWorkItemDeliveryConfirmation: Identifiable {
    let projectID: String
    let item: MordeProjectWorkItem
    let target: MIFWorkItemDeliveryTarget
    let taskID: String

    var id: String { "\(projectID):\(item.id):\(target.id):\(taskID)" }
}

@MainActor
struct MIFWorkItemRoom: View {
    @StateObject private var store = MIFWorkItemPageStore.shared
    @Environment(\.dismiss) private var dismiss
    @State private var title = ""
    @State private var description = ""
    @State private var blockingItemID: String?
    @State private var blockedReason = ""
    @State private var deliveryConfirmation: MIFWorkItemDeliveryConfirmation?
    @State private var requestGeneration = UUID()
    @State private var loadedScopeKey: String?
    @State private var loadingScope = false
    @State private var localFailure: String?

    private var selectedProject: MordeProject? {
        store.projects.first { $0.id == store.selectedProjectID }
    }

    private var canManageSelectedProject: Bool {
        guard let projectID = store.selectedProjectID else { return false }
        return store.canManage(in: projectID)
    }

    var body: some View {
        VStack(spacing: 0) {
            header
            ScrollView {
                VStack(spacing: 14) {
                    content
                }
                .padding(.vertical, 14)
                .frame(maxWidth: 680)
                .frame(maxWidth: .infinity)
            }
            .background(NanoMuseTones.canvas)
            .refreshable { await loadScope(store.scopeKey) }
        }
        .background(NanoMuseTones.canvas.ignoresSafeArea())
        .toolbar(.hidden, for: .navigationBar)
        .tint(NanoMuseTones.action)
        .task(id: store.scopeKey) { await loadScope(store.scopeKey) }
        .onChange(of: store.selectedProjectID) { _ in
            title = ""
            description = ""
            blockingItemID = nil
            blockedReason = ""
            deliveryConfirmation = nil
            localFailure = nil
        }
        .onChange(of: store.selectedConversationID) { _ in deliveryConfirmation = nil }
        .confirmationDialog("确认登记工作项交付？", isPresented: deliveryConfirmationIsPresented,
                            titleVisibility: .visible) {
            if let request = deliveryConfirmation,
               store.selectedProjectID == request.projectID,
               store.currentTask?.id == request.taskID {
                Button("登记已保存版本 v\(request.target.revision)") { recordDelivery(request) }
                    .accessibilityIdentifier("morde.workItems.delivery.confirm")
            }
            Button("取消", role: .cancel) { deliveryConfirmation = nil }
                .accessibilityIdentifier("morde.workItems.delivery.cancel")
        } message: {
            if let request = deliveryConfirmation {
                Text("\(request.item.title) · Task \(request.taskID) · \(request.target.workspaceName) v\(request.target.revision)。只登记版本元数据，不传送文件。")
            }
        }
    }

    private var header: some View {
        ZStack {
            Text("工作项")
                .font(.headline.weight(.semibold))
                .lineLimit(1)
                .padding(.horizontal, 60)
            HStack {
                Button { dismiss() } label: {
                    Image(systemName: "chevron.left")
                        .font(.system(size: 15, weight: .semibold))
                        .frame(width: 40, height: 40)
                        .background(NanoMuseTones.surface, in: Circle())
                }
                .accessibilityLabel("返回")
                .accessibilityIdentifier("morde.workItems.back")
                Spacer()
                Button { Task { await loadScope(store.scopeKey) } } label: {
                    Image(systemName: "arrow.clockwise")
                        .font(.system(size: 16, weight: .medium))
                        .frame(width: 40, height: 40)
                        .background(NanoMuseTones.surface, in: Circle())
                }
                .disabled(loadingScope || store.scopeKey == nil)
                .accessibilityLabel("刷新工作项")
                .accessibilityIdentifier("morde.workItems.refresh")
            }
            .padding(.horizontal, 12)
        }
        .frame(height: 52)
        .background(NanoMuseTones.canvas)
    }

    @ViewBuilder
    private var content: some View {
        if !store.isSignedIn {
            notice("请先登录 Morde 账号查看项目工作项。", symbol: "person.crop.circle")
        } else if store.scopeKey == nil || loadedScopeKey != store.scopeKey {
            ProgressView("正在确认账号和项目…")
                .frame(maxWidth: .infinity, minHeight: 150)
        } else if let failure = store.projectsState.failureMessage {
            errorCard(failure, retry: { Task { await loadScope(store.scopeKey) } })
        } else if !store.projectsState.isLoaded {
            ProgressView("正在读取项目…")
                .frame(maxWidth: .infinity, minHeight: 150)
        } else if store.projects.isEmpty {
            notice("当前账号尚未加入项目。", symbol: "folder")
        } else {
            projectPicker
            if let project = selectedProject {
                if let message = store.actionError ?? localFailure {
                    inlineMessage(message, isError: true)
                        .accessibilityIdentifier("morde.workItems.error")
                } else if let message = store.notice {
                    inlineMessage(message, isError: false)
                        .accessibilityIdentifier("morde.workItems.notice")
                }
                conversationPicker(projectID: project.id)
                createForm(projectID: project.id)
                workItemList(project: project)
            } else {
                notice("选择一个项目查看其中的工作项。", symbol: "folder")
            }
        }
    }

    private var projectPicker: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("项目")
                .font(.caption.weight(.medium))
                .foregroundStyle(.secondary)
            Menu {
                ForEach(store.projects) { project in
                    Button {
                        Task { await store.selectProject(project.id) }
                    } label: {
                        if project.id == store.selectedProjectID {
                            Label(project.name, systemImage: "checkmark")
                        } else {
                            Text(project.name)
                        }
                    }
                    .accessibilityIdentifier("morde.workItems.project.\(project.id)")
                }
            } label: {
                HStack(spacing: 10) {
                    Image(systemName: "folder")
                        .foregroundStyle(NanoMuseTones.action)
                    Text(selectedProject?.name ?? "选择项目")
                        .font(.body.weight(.medium))
                        .lineLimit(1)
                        .frame(maxWidth: .infinity, alignment: .leading)
                    Image(systemName: "chevron.down")
                        .font(.caption.weight(.semibold))
                        .foregroundStyle(.secondary)
                }
                .contentShape(Rectangle())
                .padding(14)
                .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 16, style: .continuous))
                .overlay(RoundedRectangle(cornerRadius: 16, style: .continuous)
                    .strokeBorder(NanoMuseTones.hairline))
            }
            .accessibilityLabel(selectedProject?.name ?? "选择项目")
            .accessibilityIdentifier("morde.workItems.projectPicker")
        }
        .padding(.horizontal, 16)
    }

    @ViewBuilder
    private func conversationPicker(projectID: String) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(spacing: 8) {
                Image(systemName: "checklist")
                    .foregroundStyle(NanoMuseTones.action)
                Text("当前项目 Task")
                    .font(.headline)
            }

            if let failure = store.conversationsState.failureMessage {
                Text("项目会话读取失败：\(failure)")
                    .font(.footnote)
                    .foregroundStyle(.orange)
                    .fixedSize(horizontal: false, vertical: true)
                Button("重试读取") { Task { await store.selectProject(projectID) } }
                    .accessibilityIdentifier("morde.workItems.conversation.retry")
            } else if !store.conversationsState.isLoaded {
                ProgressView("正在读取项目会话…")
            } else if store.conversations.isEmpty {
                Text("当前项目没有可选择的共享会话。")
                    .font(.footnote)
                    .foregroundStyle(.secondary)
            } else {
                Menu {
                    ForEach(store.conversations) { conversation in
                        Button {
                            Task { await store.selectConversation(conversation.id) }
                        } label: {
                            let label = conversation.title.isEmpty ? "新共享会话" : conversation.title
                            if conversation.id == store.selectedConversationID {
                                Label(label, systemImage: "checkmark")
                            } else {
                                Text(label)
                            }
                        }
                        .accessibilityIdentifier("morde.workItems.conversation.\(conversation.id)")
                    }
                } label: {
                    HStack(spacing: 8) {
                        Text(selectedConversationTitle)
                            .lineLimit(1)
                            .frame(maxWidth: .infinity, alignment: .leading)
                        Image(systemName: "chevron.down")
                            .font(.caption.weight(.semibold))
                            .foregroundStyle(.secondary)
                    }
                    .padding(12)
                    .background(NanoMuseTones.fill, in: RoundedRectangle(cornerRadius: 12, style: .continuous))
                }
                .accessibilityIdentifier("morde.workItems.conversationPicker")

                if let failure = store.conversationError {
                    Text(failure)
                        .font(.footnote)
                        .foregroundStyle(.orange)
                        .fixedSize(horizontal: false, vertical: true)
                        .accessibilityIdentifier("morde.workItems.conversation.error")
                } else if let task = store.currentTask {
                    VStack(alignment: .leading, spacing: 4) {
                        Text(task.objective.isEmpty ? "当前会话 Task" : task.objective)
                            .font(.subheadline.weight(.medium))
                            .fixedSize(horizontal: false, vertical: true)
                        Text("Task \(task.id) · \(task.statusLabel)")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .accessibilityIdentifier("morde.workItems.currentTask")
                } else {
                    Text("当前会话没有可用于关联的 Task，或 Task 仍在读取。")
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
        }
        .padding(16)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 18, style: .continuous))
        .overlay(RoundedRectangle(cornerRadius: 18, style: .continuous).strokeBorder(NanoMuseTones.hairline))
        .padding(.horizontal, 16)
    }

    @ViewBuilder
    private func createForm(projectID: String) -> some View {
        if store.canManage(in: projectID) {
            VStack(alignment: .leading, spacing: 10) {
                Text("新建工作项")
                    .font(.headline)
                TextField("标题", text: $title)
                    .textFieldStyle(.roundedBorder)
                    .disabled(store.actionInFlight)
                    .accessibilityIdentifier("morde.workItems.create.title")
                TextField("说明（可选）", text: $description, axis: .vertical)
                    .lineLimit(3...6)
                    .textFieldStyle(.roundedBorder)
                    .disabled(store.actionInFlight)
                    .accessibilityIdentifier("morde.workItems.create.description")
                Button {
                    create(projectID: projectID)
                } label: {
                    HStack {
                        if store.actionInFlight { ProgressView().tint(NanoMuseTones.onInk) }
                        Text("创建工作项")
                    }
                    .frame(maxWidth: .infinity, minHeight: 44)
                }
                .buttonStyle(.borderedProminent)
                .disabled(store.actionInFlight || title.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                .accessibilityIdentifier("morde.workItems.create.submit")
            }
            .padding(16)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 18, style: .continuous))
            .overlay(RoundedRectangle(cornerRadius: 18, style: .continuous).strokeBorder(NanoMuseTones.hairline))
            .padding(.horizontal, 16)
        } else {
            inlineMessage("你可以查看工作项；只有项目负责人或成员可以创建和变更。", isError: false)
                .accessibilityIdentifier("morde.workItems.readOnly")
        }
    }

    @ViewBuilder
    private func workItemList(project: MordeProject) -> some View {
        VStack(spacing: 12) {
            HStack {
                Text("工作项")
                    .font(.headline)
                Spacer()
                if store.workItemsState.isLoaded {
                    Text("\(store.workItems.count) 项")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
            .padding(.horizontal, 16)

            if let failure = store.workItemsState.failureMessage {
                errorCard(failure) { Task { await store.refreshWorkItems(project.id) } }
            } else if !store.workItemsState.isLoaded {
                ProgressView("正在读取工作项…")
                    .frame(maxWidth: .infinity, minHeight: 120)
            } else if store.workItems.isEmpty {
                notice("这个项目还没有工作项。", symbol: "checklist")
            } else {
                ForEach(store.workItems) { item in
                    workItemCard(item, projectID: project.id)
                }
            }
        }
    }

    private func workItemCard(_ item: MordeProjectWorkItem, projectID: String) -> some View {
        let role = store.role(in: projectID)
        let canManage = store.canManage(in: projectID)
        let task = store.currentTask

        return VStack(alignment: .leading, spacing: 11) {
            HStack(alignment: .top, spacing: 10) {
                VStack(alignment: .leading, spacing: 5) {
                    Text(item.title)
                        .font(.headline)
                        .fixedSize(horizontal: false, vertical: true)
                        .accessibilityIdentifier("morde.workItems.row.title.\(item.id)")
                    Text("负责人 \(item.assignee_id) · v\(item.version)")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                Spacer(minLength: 0)
                Text(MIFWorkItemPresentation.stateLabel(item.state))
                    .font(.caption.weight(.medium))
                    .foregroundStyle(item.state == "blocked" ? Color.orange : Color.secondary)
                    .padding(.horizontal, 9)
                    .padding(.vertical, 5)
                    .background(NanoMuseTones.fill, in: Capsule())
                    .accessibilityIdentifier("morde.workItems.row.state.\(item.id)")
            }

            if !item.description.isEmpty {
                Text(item.description)
                    .font(.subheadline)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if !item.acceptance_criteria.isEmpty {
                VStack(alignment: .leading, spacing: 4) {
                    Text("验收标准")
                        .font(.caption.weight(.medium))
                        .foregroundStyle(.secondary)
                    ForEach(Array(item.acceptance_criteria.enumerated()), id: \.offset) { _, criterion in
                        Text("• \(criterion)")
                            .font(.footnote)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }
            }
            if !item.blocked_reason.isEmpty {
                Text("阻塞原因：\(item.blocked_reason)")
                    .font(.footnote)
                    .foregroundStyle(.orange)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if !item.task_ids.isEmpty {
                Text("关联 Task：\(item.task_ids.joined(separator: "、"))")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if let delivery = item.delivery {
                Text("已登记交付：Task \(delivery.task_id) · Workspace \(delivery.workspace_id) v\(delivery.revision)")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }

            if canManage, let task,
               MIFWorkItemPresentation.canLinkTask(item: item, taskID: task.id, role: role) {
                Button {
                    Task { await linkTask(projectID: projectID, item: item, taskID: task.id) }
                } label: {
                    Label("关联当前 Task", systemImage: "link")
                        .frame(maxWidth: .infinity, minHeight: 42)
                }
                .buttonStyle(.bordered)
                .disabled(store.actionInFlight)
                .accessibilityIdentifier("morde.workItems.linkTask.\(item.id)")
            } else if let task, item.task_ids.contains(task.id) {
                Text("当前 Task 已关联")
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .accessibilityIdentifier("morde.workItems.linkTask.done.\(item.id)")
            }

            if canManage && item.state == "open", let task,
               store.deliveryTargets.contains(where: {
                   MIFWorkItemPresentation.canRecordDelivery(item: item, taskID: task.id,
                                                            target: $0, role: role)
               }) {
                Menu {
                    ForEach(store.deliveryTargets.filter {
                        MIFWorkItemPresentation.canRecordDelivery(item: item, taskID: task.id,
                                                                  target: $0, role: role)
                    }) { target in
                        Button("\(target.hostName) · \(target.workspaceName) v\(target.revision)") {
                            deliveryConfirmation = MIFWorkItemDeliveryConfirmation(
                                projectID: projectID, item: item, target: target, taskID: task.id)
                        }
                        .accessibilityIdentifier("morde.workItems.delivery.target.\(target.id)")
                    }
                } label: {
                    Label("登记 Workspace 版本", systemImage: "checkmark.seal")
                        .frame(maxWidth: .infinity, minHeight: 42)
                }
                .buttonStyle(.bordered)
                .disabled(store.actionInFlight)
                .accessibilityIdentifier("morde.workItems.delivery.picker.\(item.id)")
            }

            if MIFWorkItemPresentation.canBlock(item: item, canManage: canManage) {
                if blockingItemID == item.id {
                    TextField("阻塞原因", text: $blockedReason, axis: .vertical)
                        .lineLimit(2...5)
                        .textFieldStyle(.roundedBorder)
                        .disabled(store.actionInFlight)
                        .accessibilityIdentifier("morde.workItems.blockReason.\(item.id)")
                    Button("确认阻塞") { block(projectID: projectID, item: item) }
                        .buttonStyle(.bordered)
                        .disabled(store.actionInFlight || blockedReason.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                        .accessibilityIdentifier("morde.workItems.block.confirm.\(item.id)")
                } else {
                    Button("标记阻塞") { blockingItemID = item.id }
                        .buttonStyle(.bordered)
                        .disabled(store.actionInFlight)
                        .accessibilityIdentifier("morde.workItems.block.\(item.id)")
                }
            } else if MIFWorkItemPresentation.canResume(item: item, canManage: canManage) {
                Button("解除阻塞") { resume(projectID: projectID, item: item) }
                    .buttonStyle(.bordered)
                    .disabled(store.actionInFlight)
                    .accessibilityIdentifier("morde.workItems.resume.\(item.id)")
            }
        }
        .padding(16)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 18, style: .continuous))
        .overlay(RoundedRectangle(cornerRadius: 18, style: .continuous).strokeBorder(NanoMuseTones.hairline))
        .accessibilityElement(children: .contain)
        .accessibilityIdentifier("morde.workItems.row.\(item.id)")
        .padding(.horizontal, 16)
    }

    private var selectedConversationTitle: String {
        guard let selectedID = store.selectedConversationID,
              let conversation = store.conversations.first(where: { $0.id == selectedID }) else {
            return "选择项目会话"
        }
        return conversation.title.isEmpty ? "新共享会话" : conversation.title
    }

    private var deliveryConfirmationIsPresented: Binding<Bool> {
        Binding(
            get: { deliveryConfirmation != nil },
            set: { if !$0 { deliveryConfirmation = nil } }
        )
    }

    private func loadScope(_ scope: String?) async {
        let generation = UUID()
        requestGeneration = generation
        loadedScopeKey = nil
        loadingScope = true
        localFailure = nil
        defer {
            if requestGeneration == generation { loadingScope = false }
        }
        guard store.isSignedIn, let scope else { return }
        await store.load()
        guard !Task.isCancelled, requestGeneration == generation, scope == store.scopeKey else { return }
        loadedScopeKey = scope
    }

    private func create(projectID: String) {
        let submittedTitle = title.trimmingCharacters(in: .whitespacesAndNewlines)
        let submittedDescription = description
        guard !submittedTitle.isEmpty else { return }
        Task {
            localFailure = nil
            guard await store.create(projectID: projectID, title: submittedTitle,
                                     description: submittedDescription) else {
                showFallbackFailure()
                return
            }
            self.title = ""
            if description == submittedDescription { description = "" }
        }
    }

    private func block(projectID: String, item: MordeProjectWorkItem) {
        Task {
            localFailure = nil
            guard await store.block(projectID: projectID, item: item, reason: blockedReason) else {
                showFallbackFailure()
                return
            }
            blockedReason = ""
            blockingItemID = nil
        }
    }

    private func resume(projectID: String, item: MordeProjectWorkItem) {
        Task {
            localFailure = nil
            if !(await store.resume(projectID: projectID, item: item)) { showFallbackFailure() }
        }
    }

    private func linkTask(projectID: String, item: MordeProjectWorkItem, taskID: String) async {
        localFailure = nil
        if !(await store.linkTask(projectID: projectID, item: item, taskID: taskID)) { showFallbackFailure() }
    }

    private func recordDelivery(_ request: MIFWorkItemDeliveryConfirmation) {
        Task {
            localFailure = nil
            guard store.selectedProjectID == request.projectID, store.currentTask?.id == request.taskID else {
                localFailure = "项目或当前 Task 已变化；未登记交付。"
                deliveryConfirmation = nil
                return
            }
            guard store.deliveryTargets.contains(where: {
                $0.id == request.target.id && $0.revision == request.target.revision
            }) else {
                localFailure = "Workspace 版本已变化；请重新选择并确认当前版本。"
                deliveryConfirmation = nil
                return
            }
            let success = await store.recordDelivery(projectID: request.projectID, item: request.item,
                                                     target: request.target, taskID: request.taskID)
            deliveryConfirmation = nil
            if !success { showFallbackFailure() }
        }
    }

    private func showFallbackFailure() {
        if store.actionError == nil { localFailure = "操作未完成，请确认当前状态后重试。" }
    }

    private func errorCard(_ message: String, retry: @escaping () -> Void) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            Label("读取失败", systemImage: "exclamationmark.triangle")
                .font(.headline)
                .foregroundStyle(.orange)
            Text(message)
                .font(.footnote)
                .fixedSize(horizontal: false, vertical: true)
                .accessibilityIdentifier("morde.workItems.loadError.message")
            Button("重试读取", action: retry)
                .buttonStyle(.bordered)
                .accessibilityIdentifier("morde.workItems.retry")
        }
        .padding(16)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 18, style: .continuous))
        .overlay(RoundedRectangle(cornerRadius: 18, style: .continuous).strokeBorder(NanoMuseTones.hairline))
        .padding(.horizontal, 16)
        .accessibilityElement(children: .contain)
        .accessibilityIdentifier("morde.workItems.loadError")
    }

    private func inlineMessage(_ message: String, isError: Bool) -> some View {
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: isError ? "exclamationmark.circle" : "checkmark.circle")
                .foregroundStyle(isError ? Color.orange : NanoMuseTones.action)
            Text(message)
                .font(.footnote)
                .fixedSize(horizontal: false, vertical: true)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
        .padding(12)
        .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 14, style: .continuous))
        .padding(.horizontal, 16)
        .accessibilityElement(children: .combine)
    }

    private func notice(_ message: String, symbol: String) -> some View {
        VStack(spacing: 9) {
            Image(systemName: symbol)
                .font(.system(size: 29))
                .foregroundStyle(NanoMuseTones.action)
            Text(message)
                .font(.subheadline)
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
                .fixedSize(horizontal: false, vertical: true)
        }
        .frame(maxWidth: .infinity, minHeight: 150)
        .padding(20)
        .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 18, style: .continuous))
        .padding(.horizontal, 16)
    }
}
