import SwiftUI
import UIKit

private enum MIFWorkItemInputField: Hashable {
    case description
    case blockReason
}

private struct MIFWorkItemTitleInput: View {
    @Binding var title: String
    let ink: Color
    let placeholderColor: Color
    let border: Color
    let isDisabled: Bool
    let focusRequest: Int
    @FocusState private var isFocused: Bool

    var body: some View {
        TextField(
            AppLocalized("Describe the issue or request in one sentence"),
            text: $title,
            prompt: Text(AppLocalized("Describe the issue or request in one sentence"))
                .foregroundColor(placeholderColor)
        )
        .textFieldStyle(.plain)
        .font(.system(size: 15))
        .foregroundStyle(ink)
        .focused($isFocused)
        .onChange(of: focusRequest) { _ in isFocused = true }
        .disabled(isDisabled)
        .padding(.horizontal, 15)
        .frame(minHeight: 44)
        .background(NanoMuseTones.surface, in: Capsule())
        .overlay(Capsule().strokeBorder(border))
        .accessibilityIdentifier("morde.workItems.create.title")
    }
}

enum MIFWorkItemFilter: String, CaseIterable, Identifiable {
    case all
    case bug
    case request
    case unclaimed

    var id: String { rawValue }

    var label: String {
        switch self {
        case .all: return AppLocalized("All")
        case .bug: return AppLocalized("Bug")
        case .request: return AppLocalized("Request")
        case .unclaimed: return AppLocalized("Unclaimed")
        }
    }
}

enum MIFWorkItemPresentation {
    static func stateLabel(_ state: String) -> String {
        switch state {
        case "open": return AppLocalized("Open status")
        case "blocked": return AppLocalized("Blocked")
        case "delivered": return AppLocalized("Delivered")
        default: return AppLocalized("Status unknown")
        }
    }

    static func canManage(role: String?) -> Bool {
        MordeProjectPolicy.canManageWorkItems(role: role)
    }

    static func categoryLabel(_ category: String?) -> String {
        guard let category else { return AppLocalized("Unclassified") }
        switch category {
        case "bug": return AppLocalized("Bug")
        case "request": return AppLocalized("Request")
        case "unclassified": return AppLocalized("Unclassified")
        default: return category
        }
    }

    static func categoryMarkerLabel(_ category: String?) -> String? {
        switch category {
        case "bug": return "缺"
        case "request": return "需"
        default: return nil
        }
    }

    static func assigneeLabel(_ assigneeID: String?) -> String {
        guard let assigneeID, !assigneeID.isEmpty else { return AppLocalized("Unclaimed") }
        return String(format: AppLocalized("Assigned to %@"), assigneeID)
    }

    static func matches(_ item: MordeProjectWorkItem, filter: MIFWorkItemFilter) -> Bool {
        matches(filter: filter, category: item.category, assigneeID: item.assignee_id)
    }

    static func matches(filter: MIFWorkItemFilter, category: String?, assigneeID: String?) -> Bool {
        switch filter {
        case .all: return true
        case .bug: return category == "bug"
        case .request: return category == "request"
        case .unclaimed: return assigneeID == nil
        }
    }

    static func canCreate(supportsV2: Bool, role: String?) -> Bool {
        supportsV2 && canManage(role: role)
    }

    static func canClaim(item: MordeProjectWorkItem, canClaim: Bool, role: String?) -> Bool {
        canClaim && role != "viewer" && item.assignee_id == nil && item.state != "delivered"
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
    var chrome: NanoMuseRoomChrome? = nil
    var onMenu: (() -> Void)? = nil
    var workItemSection: Binding<MIFTaskWorkItemSection>? = nil
    @StateObject private var store = MIFWorkItemPageStore.shared
    @Environment(\.dismiss) private var dismiss
    @Environment(\.colorScheme) private var colorScheme
    @State private var title = ""
    @State private var description = ""
    @FocusState private var focusedInput: MIFWorkItemInputField?
    @State private var titleFocusRequest = 0
    @State private var createCategory = "bug"
    @State private var isCreatePresented = false
    @State private var selectedFilter: MIFWorkItemFilter = .all
    @State private var blockingItemID: String?
    @State private var blockedReason = ""
    @State private var deliveryConfirmation: MIFWorkItemDeliveryConfirmation?
    @State private var requestGeneration = UUID()
    @State private var loadedScopeKey: String?
    @State private var localFailure: String?
    @State private var createFailure: String?
    @State private var createSuccessToast: String?
    @State private var createSuccessToastID = UUID()

    private var workItemSecondaryText: Color {
        colorScheme == .dark
            ? NanoMuseTones.secondaryText
            : Color(red: 115.0 / 255.0, green: 115.0 / 255.0, blue: 120.0 / 255.0)
    }

    private var workItemInk: Color {
        colorScheme == .dark
            ? .white
            : Color(red: 17.0 / 255.0, green: 17.0 / 255.0, blue: 17.0 / 255.0)
    }

    private var workItemSheetBackground: Color {
        colorScheme == .dark
            ? NanoMuseTones.canvas
            : Color(red: 250.0 / 255.0, green: 250.0 / 255.0, blue: 250.0 / 255.0)
    }

    private var workItemInputBorder: Color {
        colorScheme == .dark
            ? NanoMuseTones.hairline
            : Color(red: 232.0 / 255.0, green: 232.0 / 255.0, blue: 232.0 / 255.0)
    }

    private var workItemStatusText: Color {
        colorScheme == .dark
            ? Color(uiColor: UIColor.tertiaryLabel)
            : Color(red: 172.0 / 255.0, green: 172.0 / 255.0, blue: 177.0 / 255.0)
    }

    private var workItemNeutralFill: Color {
        colorScheme == .dark
            ? NanoMuseTones.fill
            : Color(red: 236.0 / 255.0, green: 236.0 / 255.0, blue: 240.0 / 255.0)
    }

    private var workItemBugFill: Color {
        colorScheme == .dark
            ? Color.red.opacity(0.1)
            : Color(red: 253.0 / 255.0, green: 236.0 / 255.0, blue: 236.0 / 255.0)
    }

    private var workItemBugText: Color {
        colorScheme == .dark
            ? Color.red
            : Color(red: 200.0 / 255.0, green: 30.0 / 255.0, blue: 30.0 / 255.0)
    }

    private var workItemFilterFill: Color {
        colorScheme == .dark
            ? NanoMuseTones.fill
            : Color(red: 239.0 / 255.0, green: 239.0 / 255.0, blue: 239.0 / 255.0)
    }

    private var workItemTypeToggleFill: Color {
        colorScheme == .dark
            ? NanoMuseTones.fill
            : Color(red: 240.0 / 255.0, green: 240.0 / 255.0, blue: 242.0 / 255.0)
    }

    private var selectedProject: MordeProject? {
        store.projects.first { $0.id == store.selectedProjectID }
    }

    private var workItemsUnsupportedNotice: some View {
        notice(AppLocalized("This service does not support the current work item features."),
               symbol: "exclamationmark.triangle")
            .accessibilityIdentifier("morde.workItems.unsupported")
    }

    var body: some View {
        VStack(spacing: 0) {
            if let onMenu {
                MordeDesignPageHeader(title: AppLocalized("Work items"), onMenu: onMenu) {
                    newWorkItemButton
                }
            } else if let chrome {
                NanoMuseRoomHeader(chrome: chrome) { newWorkItemButton }
            } else {
                header
            }
            if let workItemSection {
                MIFTaskWorkItemSectionPicker(selection: workItemSection)
            }
            ScrollView {
                VStack(spacing: 14) {
                    content
                }
                .padding(.top, 2)
                .padding(.bottom, onMenu != nil ? 95 : 14)
                .padding(.bottom, onMenu == nil && chrome != nil ? NanoMuseBottomBar.height + 8 : 0)
                .frame(maxWidth: 680)
                .frame(maxWidth: .infinity)
            }
            .background(NanoMuseTones.canvas)
            .refreshable {
                let refreshTask = Task { await loadScope(store.scopeKey) }
                await refreshTask.value
            }
            .accessibilityIdentifier("morde.workItems.scroll")
        }
        .background(NanoMuseTones.canvas.ignoresSafeArea())
        .toolbar(.hidden, for: .navigationBar)
        .toolbar {
            ToolbarItemGroup(placement: .keyboard) {
                Spacer()
                Button(AppLocalized("Done")) { focusedInput = nil }
                    .accessibilityIdentifier("morde.workItems.keyboardDone")
            }
        }
        .tint(NanoMuseTones.action)
        .sheet(isPresented: $isCreatePresented) {
            if let project = selectedProject {
                ScrollView {
                    createForm(projectID: project.id)
                }
                .accessibilityIdentifier("morde.workItems.create.scroll")
                .background(workItemSheetBackground.ignoresSafeArea())
                .presentationDetents([.medium, .large])
                .presentationDragIndicator(.hidden)
                .toolbar {
                    ToolbarItemGroup(placement: .keyboard) {
                        Spacer()
                        Button(AppLocalized("Done")) { focusedInput = nil }
                            .accessibilityIdentifier("morde.workItems.keyboardDone")
                    }
                }
            }
        }
        .task(id: store.scopeKey) { await loadScope(store.scopeKey) }
        .onChange(of: store.selectedProjectID) { _ in
            title = ""
            description = ""
            selectedFilter = .all
            createCategory = "bug"
            blockingItemID = nil
            blockedReason = ""
            deliveryConfirmation = nil
            localFailure = nil
            createFailure = nil
            isCreatePresented = false
            createSuccessToastID = UUID()
            createSuccessToast = nil
        }
        .onChange(of: store.selectedConversationID) { _ in deliveryConfirmation = nil }
        .confirmationDialog(AppLocalized("Record work item delivery?"), isPresented: deliveryConfirmationIsPresented,
                            titleVisibility: .visible) {
            if let request = deliveryConfirmation,
               store.selectedProjectID == request.projectID,
               store.currentTask?.id == request.taskID {
                Button(String(format: AppLocalized("Record saved version v%lld"), request.target.revision)) {
                    recordDelivery(request)
                }
                    .accessibilityIdentifier("morde.workItems.delivery.confirm")
            }
            Button(AppLocalized("Cancel"), role: .cancel) { deliveryConfirmation = nil }
                .accessibilityIdentifier("morde.workItems.delivery.cancel")
        } message: {
            if let request = deliveryConfirmation {
                Text(String(format: AppLocalized("%1$@ · Task %2$@ · %3$@ v%4$lld. Only version metadata will be recorded; no files will be sent."),
                            request.item.title, request.taskID, request.target.workspaceName, request.target.revision))
            }
        }
        .overlay(alignment: .bottom) {
            if let createSuccessToast {
                Text(createSuccessToast)
                    .font(.subheadline)
                    .foregroundStyle(.white)
                    .padding(.horizontal, 16)
                    .padding(.vertical, 10)
                    .background(Color.black.opacity(0.82), in: Capsule())
                    .padding(.bottom, (onMenu != nil || chrome != nil) ? NanoMuseBottomBar.height + 24 : 24)
                    .accessibilityIdentifier("morde.workItems.create.successToast")
                    .transition(.opacity)
            }
        }
    }

    private var header: some View {
        ZStack {
            Text(AppLocalized("Work items"))
                .font(.headline.weight(.semibold))
                .lineLimit(nil)
                .multilineTextAlignment(.center)
                .fixedSize(horizontal: false, vertical: true)
                .padding(.horizontal, 60)
            HStack {
                Button { dismiss() } label: {
                    Image(systemName: "chevron.left")
                        .font(.system(size: 15, weight: .semibold))
                        .frame(width: 40, height: 40)
                        .background(NanoMuseTones.surface, in: Circle())
                }
                .accessibilityLabel(AppLocalized("Back"))
                .accessibilityIdentifier("morde.workItems.back")
                Spacer()
                Button { presentCreateForm() } label: {
                    Image(systemName: "plus")
                        .font(.system(size: 16, weight: .medium))
                        .frame(width: 40, height: 40)
                        .background(NanoMuseTones.surface, in: Circle())
                }
                .disabled(!canCreateWorkItem)
                .accessibilityLabel(AppLocalized("New work item"))
                .accessibilityIdentifier("morde.workItems.create.open")
            }
            .padding(.horizontal, 12)
        }
        .frame(minHeight: 52)
        .background(NanoMuseTones.canvas)
    }

    private var canCreateWorkItem: Bool {
        guard store.isSignedIn, let scopeKey = store.scopeKey, loadedScopeKey == scopeKey,
              store.projectsState.isLoaded, store.projectsState.failureMessage == nil,
              let project = selectedProject else { return false }
        return MIFWorkItemPresentation.canCreate(
            supportsV2: store.supportsWorkItemsV2, role: store.role(in: project.id))
    }

    private var newWorkItemButton: some View {
        Button { presentCreateForm() } label: {
            Image(systemName: "plus")
                .font(.system(size: 19, weight: .regular))
                .foregroundStyle(workItemInk)
                .frame(width: 38, height: 38)
                .background(NanoMuseTones.surface, in: Circle())
                .overlay(Circle().strokeBorder(workItemInputBorder))
                .contentShape(Circle())
        }
        .disabled(!canCreateWorkItem)
        .accessibilityLabel(AppLocalized("New work item"))
        .accessibilityIdentifier("morde.workItems.create.open")
    }

    @ViewBuilder
    private var content: some View {
        if !store.isSignedIn {
            notice(AppLocalized("Sign in to view project work items"), symbol: "person.crop.circle")
        } else if store.scopeKey == nil || loadedScopeKey != store.scopeKey {
            ProgressView(AppLocalized("Confirming account and projects…"))
                .frame(maxWidth: .infinity, minHeight: 150)
        } else if let failure = store.projectsState.failureMessage {
            errorCard(failure)
                .accessibilityIdentifier("morde.workItems.projects.loadError")
        } else if !store.projectsState.isLoaded {
            ProgressView(AppLocalized("Loading projects…"))
                .frame(maxWidth: .infinity, minHeight: 150)
        } else if store.projects.isEmpty {
            notice(AppLocalized("This account has not joined any projects."), symbol: "folder")
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
                conversationPicker()
                workItemList(project: project)
            } else {
                notice(AppLocalized("Choose a project to view its work items."), symbol: "folder")
            }
        }
    }

    private var projectPicker: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text(AppLocalized("Project"))
                .font(.caption.weight(.medium))
                .foregroundStyle(NanoMuseTones.secondaryText)
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
                    Text(selectedProject?.name ?? AppLocalized("Choose project"))
                        .font(.body.weight(.medium))
                        .foregroundStyle(Color.primary)
                        .fixedSize(horizontal: false, vertical: true)
                        .frame(maxWidth: .infinity, alignment: .leading)
                    Image(systemName: "chevron.down")
                        .font(.caption.weight(.semibold))
                        .foregroundStyle(NanoMuseTones.secondaryText)
                }
                .contentShape(Rectangle())
                .padding(14)
                .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 16, style: .continuous))
                .overlay(RoundedRectangle(cornerRadius: 16, style: .continuous)
                    .strokeBorder(NanoMuseTones.hairline))
            }
            .accessibilityLabel(selectedProject?.name ?? AppLocalized("Choose project"))
            .accessibilityIdentifier("morde.workItems.projectPicker")
        }
        .padding(.horizontal, 16)
    }

    @ViewBuilder
    private func conversationPicker() -> some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(spacing: 8) {
                Image(systemName: "checklist")
                    .foregroundStyle(NanoMuseTones.action)
                Text(AppLocalized("Task in this project"))
                    .font(.headline)
            }

            if let failure = store.conversationsState.failureMessage {
                Text(String(format: AppLocalized("Could not load project conversations: %@"), failure))
                    .font(.footnote)
                    .foregroundStyle(.orange)
                    .fixedSize(horizontal: false, vertical: true)
                    .accessibilityIdentifier("morde.workItems.conversations.loadError")
            } else if !store.conversationsState.isLoaded {
                ProgressView(AppLocalized("Loading project conversations…"))
            } else if store.conversations.isEmpty {
                Text(AppLocalized("This project has no shared conversations to choose from."))
                    .font(.footnote)
                    .foregroundStyle(NanoMuseTones.secondaryText)
            } else {
                Menu {
                    ForEach(store.conversations) { conversation in
                        Button {
                            Task { await store.selectConversation(conversation.id) }
                        } label: {
                            let label = conversation.title.isEmpty
                                ? AppLocalized("New shared conversation") : conversation.title
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
                            .fixedSize(horizontal: false, vertical: true)
                            .frame(maxWidth: .infinity, alignment: .leading)
                        Image(systemName: "chevron.down")
                            .font(.caption.weight(.semibold))
                            .foregroundStyle(NanoMuseTones.secondaryText)
                    }
                    .padding(12)
                    .background(NanoMuseTones.fill, in: RoundedRectangle(cornerRadius: 12, style: .continuous))
                }
                .accessibilityIdentifier("morde.workItems.conversationPicker")

                if !store.conversationTaskChoices.isEmpty {
                    Menu {
                        ForEach(store.conversationTaskChoices) { task in
                            Button {
                                Task { await store.selectTask(task.id) }
                            } label: {
                                Text("\(task.objective) · \(task.id)")
                            }
                            .accessibilityIdentifier("morde.workItems.task.\(task.id)")
                        }
                    } label: {
                        HStack {
                            Text(store.currentTask?.objective ?? AppLocalized("Task in this project"))
                                .frame(maxWidth: .infinity, alignment: .leading)
                            Image(systemName: "chevron.down")
                        }
                        .padding(12)
                        .background(NanoMuseTones.fill, in: RoundedRectangle(cornerRadius: 12, style: .continuous))
                    }
                    .accessibilityIdentifier("morde.workItems.taskPicker")
                }

                if let failure = store.conversationError {
                    Text(failure)
                        .font(.footnote)
                        .foregroundStyle(.orange)
                        .fixedSize(horizontal: false, vertical: true)
                        .accessibilityIdentifier("morde.workItems.conversation.error")
                } else if let task = store.currentTask {
                    VStack(alignment: .leading, spacing: 4) {
                        Text(task.objective.isEmpty ? AppLocalized("Task in current conversation") : task.objective)
                            .font(.subheadline.weight(.medium))
                            .fixedSize(horizontal: false, vertical: true)
                        Text(String(format: AppLocalized("Task %1$@ · %2$@"), task.id, task.statusLabel))
                            .font(.caption)
                            .foregroundStyle(NanoMuseTones.secondaryText)
                    }
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .accessibilityIdentifier("morde.workItems.currentTask")
                } else if store.conversationTaskChoices.isEmpty {
                    Text(AppLocalized("This conversation has no Task to link, or its Task is still loading."))
                        .font(.footnote)
                        .foregroundStyle(NanoMuseTones.secondaryText)
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
        if !store.supportsWorkItemsV2 {
            workItemsUnsupportedNotice
        } else if store.canManage(in: projectID) {
            VStack(alignment: .leading, spacing: 14) {
                Text(AppLocalized("New work item"))
                    .font(.system(size: 17.5, weight: .bold))
                    .tracking(-0.175)
                    .foregroundStyle(workItemInk)
                    .accessibilityIdentifier("morde.workItems.create.heading")
                VStack(spacing: 12) {
                    HStack(spacing: 8) {
                        createCategoryButton("bug", title: AppLocalized("Bug"))
                        createCategoryButton("request", title: AppLocalized("Request"))
                    }
                    workItemTitleInput
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                TextField(AppLocalized("Description (optional)"), text: $description, axis: .vertical)
                    .lineLimit(3...6)
                    .textFieldStyle(.plain)
                    .focused($focusedInput, equals: .description)
                    .disabled(store.actionInFlight)
                    .padding(14)
                    .frame(maxWidth: .infinity, minHeight: 84, alignment: .topLeading)
                    .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 16, style: .continuous))
                    .overlay(RoundedRectangle(cornerRadius: 16, style: .continuous)
                        .strokeBorder(NanoMuseTones.hairline))
                    .accessibilityIdentifier("morde.workItems.create.description")
                if let error = createFailure {
                    Text(error)
                        .font(.footnote)
                        .foregroundStyle(.orange)
                        .fixedSize(horizontal: false, vertical: true)
                        .accessibilityIdentifier("morde.workItems.create.error")
                }
                Button {
                    if title.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                        titleFocusRequest += 1
                        return
                    }
                    create(projectID: projectID)
                } label: {
                    HStack {
                        if store.actionInFlight { ProgressView().tint(NanoMuseTones.onInk) }
                        Text(AppLocalized("Create work item"))
                    }
                    .font(.system(size: 15.5, weight: .semibold))
                    .foregroundStyle(NanoMuseTones.onInk)
                    .frame(maxWidth: .infinity, minHeight: 44)
                    .background(workItemInk, in: Capsule())
                }
                .buttonStyle(.plain)
                .disabled(store.actionInFlight)
                .accessibilityIdentifier("morde.workItems.create.submit")
            }
            .padding(22)
            .padding(.bottom, 8)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(workItemSheetBackground)
        } else {
            inlineMessage(AppLocalized("You can view work items. Only project owners and members can create or change them."),
                          isError: false)
                .accessibilityIdentifier("morde.workItems.readOnly")
        }
    }

    private var workItemTitleInput: some View {
        MIFWorkItemTitleInput(title: $title, ink: workItemInk,
                              placeholderColor: workItemStatusText,
                              border: workItemInputBorder,
                              isDisabled: store.actionInFlight,
                              focusRequest: titleFocusRequest)
    }

    @ViewBuilder
    private func workItemList(project: MordeProject) -> some View {
        VStack(spacing: 0) {
            if !store.supportsWorkItemsV2 {
                workItemsUnsupportedNotice
            } else {
                filterPicker

                if let failure = store.workItemsState.failureMessage {
                    errorCard(failure)
                        .accessibilityIdentifier("morde.workItems.list.loadError")
                } else if !store.workItemsState.isLoaded {
                    ProgressView(AppLocalized("Loading work items…"))
                        .frame(maxWidth: .infinity, minHeight: 120)
                } else if store.workItems.isEmpty {
                    workItemEmptyState(AppLocalized("This project has no work items yet."))
                } else if visibleWorkItems.isEmpty {
                    workItemEmptyState(AppLocalized("No work items match this filter."))
                } else {
                    ForEach(Array(visibleWorkItems.indices), id: \.self) { index in
                        workItemCard(visibleWorkItems[index], projectID: project.id)
                    }
                }
            }
        }
    }

    private var visibleWorkItems: [MordeProjectWorkItem] {
        return store.workItems.filter { MIFWorkItemPresentation.matches($0, filter: selectedFilter) }
    }

    private var filterPicker: some View {
        HStack(spacing: 4) {
            ForEach(MIFWorkItemFilter.allCases) { filter in
                Button {
                    selectedFilter = filter
                } label: {
                    Text(filter.label)
                        .font(.system(size: 14.5, weight: .semibold))
                        .foregroundStyle(selectedFilter == filter ? workItemInk : workItemSecondaryText)
                        .padding(.horizontal, 13)
                        .padding(.vertical, 7)
                        .background {
                            if selectedFilter == filter {
                                Capsule().fill(workItemFilterFill)
                            }
                        }
                        .contentShape(Capsule())
                }
                .buttonStyle(.plain)
                .accessibilityAddTraits(selectedFilter == filter ? .isSelected : [])
                .accessibilityIdentifier("morde.workItems.filter.\(filter.rawValue)")
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(.horizontal, 22)
        .padding(.top, 8)
        .padding(.bottom, 2)
    }

    private func workItemEmptyState(_ message: String) -> some View {
        Text(message)
            .font(.system(size: 14.5))
            .foregroundStyle(workItemSecondaryText)
            .fixedSize(horizontal: false, vertical: true)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.horizontal, 22)
            .padding(.top, 7)
    }

    private func workItemCard(_ item: MordeProjectWorkItem, projectID: String) -> some View {
        let role = store.role(in: projectID)
        let canManage = store.canManage(in: projectID)
        let task = store.currentTask

        return VStack(alignment: .leading, spacing: 11) {
            HStack(alignment: .center, spacing: 13) {
                HStack(alignment: .top, spacing: 13) {
                    workItemIcon(item)
                    VStack(alignment: .leading, spacing: 2) {
                        Text(item.title)
                            .font(.system(size: 16, weight: .semibold))
                            .foregroundStyle(workItemInk)
                            .tracking(-0.16)
                            .lineSpacing(1.5)
                            .fixedSize(horizontal: false, vertical: true)
                            .accessibilityIdentifier("morde.workItems.row.title.\(item.id)")
                        Text("\(MIFWorkItemPresentation.categoryLabel(item.category)) · \(MIFWorkItemPresentation.assigneeLabel(item.assignee_id))")
                            .font(.system(size: 14))
                            .lineSpacing(3.5)
                            .foregroundStyle(workItemSecondaryText)
                            .accessibilityIdentifier("morde.workItems.row.metadata.\(item.id)")
                        Text(MIFWorkItemPresentation.stateLabel(item.state))
                            .font(.system(size: 13))
                            .foregroundStyle(workItemStatusText)
                            .padding(.top, 1)
                            .accessibilityIdentifier("morde.workItems.row.state.\(item.id)")
                    }
                    .padding(.top, 1)
                }
                Spacer(minLength: 0)
                if MIFWorkItemPresentation.canClaim(item: item,
                                                    canClaim: store.canClaim(in: projectID), role: role) {
                    Button {
                        Task { await claim(projectID: projectID, item: item) }
                    } label: {
                        Text(AppLocalized("Claim work item"))
                            .font(.system(size: 13.5, weight: .semibold))
                            .foregroundStyle(NanoMuseTones.onInk)
                            .padding(.horizontal, 15)
                            .padding(.vertical, 8)
                            .background(workItemInk, in: Capsule())
                    }
                    .buttonStyle(.plain)
                    .disabled(store.actionInFlight)
                    .accessibilityIdentifier("morde.workItems.claim.\(item.id)")
                }
            }

            if !item.description.isEmpty {
                Text(item.description)
                    .font(.subheadline)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if !item.acceptance_criteria.isEmpty {
                VStack(alignment: .leading, spacing: 4) {
                    Text(AppLocalized("Acceptance criteria"))
                        .font(.caption.weight(.medium))
                        .foregroundStyle(NanoMuseTones.secondaryText)
                    ForEach(Array(item.acceptance_criteria.enumerated()), id: \.offset) { _, criterion in
                        Text("• \(criterion)")
                            .font(.footnote)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }
            }
            if !item.blocked_reason.isEmpty {
                Text(String(format: AppLocalized("Blocked reason: %@"), item.blocked_reason))
                    .font(.footnote)
                    .foregroundStyle(.orange)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if !item.task_ids.isEmpty {
                Text(String(format: AppLocalized("Linked Tasks: %@"), item.task_ids.joined(separator: ", ")))
                    .font(.caption)
                    .foregroundStyle(NanoMuseTones.secondaryText)
                    .fixedSize(horizontal: false, vertical: true)
            }
            if let delivery = item.delivery {
                Text(String(format: AppLocalized("Recorded delivery: Task %1$@ · Workspace %2$@ v%3$lld"),
                            delivery.task_id, delivery.workspace_id, delivery.revision))
                    .font(.caption)
                    .foregroundStyle(NanoMuseTones.secondaryText)
                    .fixedSize(horizontal: false, vertical: true)
            }

            if canManage, let task,
               MIFWorkItemPresentation.canLinkTask(item: item, taskID: task.id, role: role) {
                Button {
                    Task { await linkTask(projectID: projectID, item: item, taskID: task.id) }
                } label: {
                    Label(AppLocalized("Link current Task"), systemImage: "link")
                        .frame(maxWidth: .infinity, minHeight: 42)
                }
                .buttonStyle(.bordered)
                .disabled(store.actionInFlight)
                .accessibilityIdentifier("morde.workItems.linkTask.\(item.id)")
            } else if let task, item.task_ids.contains(task.id) {
                Text(AppLocalized("Current Task is already linked"))
                    .font(.caption)
                    .foregroundStyle(NanoMuseTones.secondaryText)
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
                        Button(String(format: AppLocalized("%1$@ · %2$@ v%3$lld"),
                                      target.hostName, target.workspaceName, target.revision)) {
                            deliveryConfirmation = MIFWorkItemDeliveryConfirmation(
                                projectID: projectID, item: item, target: target, taskID: task.id)
                        }
                        .accessibilityIdentifier("morde.workItems.delivery.target.\(target.id)")
                    }
                } label: {
                    Label(AppLocalized("Record Workspace version"), systemImage: "checkmark.seal")
                        .frame(maxWidth: .infinity, minHeight: 42)
                }
                .buttonStyle(.bordered)
                .disabled(store.actionInFlight)
                .accessibilityIdentifier("morde.workItems.delivery.picker.\(item.id)")
            }

            if MIFWorkItemPresentation.canBlock(item: item, canManage: canManage) {
                if blockingItemID == item.id {
                    TextField(AppLocalized("Reason for blocking"), text: $blockedReason, axis: .vertical)
                        .lineLimit(2...5)
                        .textFieldStyle(.roundedBorder)
                        .focused($focusedInput, equals: .blockReason)
                        .disabled(store.actionInFlight)
                        .accessibilityIdentifier("morde.workItems.blockReason.\(item.id)")
                    Button(AppLocalized("Confirm block")) { block(projectID: projectID, item: item) }
                        .buttonStyle(.bordered)
                        .disabled(store.actionInFlight || blockedReason.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                        .accessibilityIdentifier("morde.workItems.block.confirm.\(item.id)")
                } else {
                    Button(AppLocalized("Mark blocked")) { blockingItemID = item.id }
                        .buttonStyle(.bordered)
                        .disabled(store.actionInFlight)
                        .accessibilityIdentifier("morde.workItems.block.\(item.id)")
                }
            } else if MIFWorkItemPresentation.canResume(item: item, canManage: canManage) {
                Button(AppLocalized("Unblock")) { resume(projectID: projectID, item: item) }
                    .buttonStyle(.bordered)
                    .disabled(store.actionInFlight)
                    .accessibilityIdentifier("morde.workItems.resume.\(item.id)")
            }
        }
        .padding(.horizontal, 22)
        .padding(.vertical, 13)
        .frame(maxWidth: .infinity, alignment: .leading)
        .accessibilityElement(children: .contain)
        .accessibilityIdentifier("morde.workItems.row.\(item.id)")
    }

    private func workItemIcon(_ item: MordeProjectWorkItem) -> some View {
        let isBug = item.category == "bug"
        let foreground = isBug ? workItemBugText : workItemInk
        let background = isBug ? workItemBugFill : workItemNeutralFill
        return Group {
            if let marker = MIFWorkItemPresentation.categoryMarkerLabel(item.category) {
                Text(marker).font(.system(size: 15, weight: .bold))
            } else {
                Image(systemName: "checklist").font(.system(size: 17, weight: .semibold))
            }
        }
            .foregroundStyle(foreground)
            .frame(width: 44, height: 44)
            .background(background, in: Circle())
            .accessibilityHidden(true)
    }

    private func createCategoryButton(_ category: String, title: String) -> some View {
        Button {
            createCategory = category
        } label: {
            Text(title)
                .font(.system(size: 15, weight: .semibold))
                .foregroundStyle(createCategory == category ? NanoMuseTones.onInk : workItemSecondaryText)
                .frame(maxWidth: .infinity, minHeight: 40)
                .background(createCategory == category ? workItemInk : workItemTypeToggleFill, in: Capsule())
        }
        .buttonStyle(.plain)
        .disabled(store.actionInFlight)
        .accessibilityAddTraits(createCategory == category ? .isSelected : [])
        .accessibilityIdentifier("morde.workItems.create.category.\(category)")
    }

    private var selectedConversationTitle: String {
        guard let selectedID = store.selectedConversationID,
              let conversation = store.conversations.first(where: { $0.id == selectedID }) else {
            return AppLocalized("Choose project conversation")
        }
        return conversation.title.isEmpty ? AppLocalized("New shared conversation") : conversation.title
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
        localFailure = nil
        guard store.isSignedIn, let scope else { return }
        await store.load()
        guard !Task.isCancelled, requestGeneration == generation, scope == store.scopeKey else { return }
        loadedScopeKey = scope
    }

    private func create(projectID: String) {
        guard store.supportsWorkItemsV2 else { return }
        let submittedTitle = title.trimmingCharacters(in: .whitespacesAndNewlines)
        let submittedDescription = description
        let submittedCategory = createCategory
        guard !submittedTitle.isEmpty else { return }
        Task {
            localFailure = nil
            createFailure = nil
            guard await store.create(projectID: projectID, title: submittedTitle,
                                     description: submittedDescription,
                                     category: submittedCategory, unassigned: true) else {
                createFailure = store.actionError
                    ?? AppLocalized("The action did not complete. Check the current state and try again.")
                return
            }
            self.title = ""
            if description == submittedDescription { description = "" }
            createFailure = nil
            isCreatePresented = false
            selectedFilter = .all
            store.clearNotice()
            showCreateSuccessToast()
        }
    }

    private func showCreateSuccessToast() {
        let toastID = UUID()
        createSuccessToastID = toastID
        withAnimation { createSuccessToast = AppLocalized("Created · visible to team") }
        Task { @MainActor in
            try? await Task.sleep(nanoseconds: 2_500_000_000)
            guard createSuccessToastID == toastID else { return }
            withAnimation { createSuccessToast = nil }
        }
    }

    private func presentCreateForm() {
        guard canCreateWorkItem else { return }
        createFailure = nil
        isCreatePresented = true
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

    private func claim(projectID: String, item: MordeProjectWorkItem) async {
        localFailure = nil
        if !(await store.claim(projectID: projectID, item: item)) { showFallbackFailure() }
    }

    private func recordDelivery(_ request: MIFWorkItemDeliveryConfirmation) {
        Task {
            localFailure = nil
            guard store.selectedProjectID == request.projectID, store.currentTask?.id == request.taskID else {
                localFailure = AppLocalized("The project or current Task changed; delivery was not recorded.")
                deliveryConfirmation = nil
                return
            }
            guard store.deliveryTargets.contains(where: {
                $0.id == request.target.id && $0.revision == request.target.revision
            }) else {
                localFailure = AppLocalized("The Workspace version changed. Select and confirm the current version again.")
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
        if store.actionError == nil {
            localFailure = AppLocalized("The action did not complete. Check the current state and try again.")
        }
    }

    private func errorCard(_ message: String) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            Label(AppLocalized("Could not load"), systemImage: "exclamationmark.triangle")
                .font(.headline)
                .foregroundStyle(.orange)
            Text(message)
                .font(.footnote)
                .fixedSize(horizontal: false, vertical: true)
                .accessibilityIdentifier("morde.workItems.loadError.message")
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
                .accessibilityHidden(true)
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
                .accessibilityHidden(true)
            Text(message)
                .font(.subheadline)
                .foregroundStyle(NanoMuseTones.secondaryText)
                .multilineTextAlignment(.center)
                .fixedSize(horizontal: false, vertical: true)
        }
        .frame(maxWidth: .infinity, minHeight: 150)
        .padding(20)
        .background(NanoMuseTones.surface, in: RoundedRectangle(cornerRadius: 18, style: .continuous))
        .padding(.horizontal, 16)
        .accessibilityElement(children: .combine)
    }
}
