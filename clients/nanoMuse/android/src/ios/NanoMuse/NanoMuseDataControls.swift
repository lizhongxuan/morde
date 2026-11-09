//
//  NanoMuseDataControls.swift
//  nanoMuse
//
//  Data controls for a nanoMuse Cloud account: what the relay holds of the account's chats
//  (the record, relay 0.29), whether their text also joins the training set for the
//  community's model, how many turns are kept, and one delete for all of it.
//  Android: ui/cloud/DataControlsScreen.kt.
//

import SwiftUI

struct NanoMuseDataControlsView: View {
    @State private var info: NanoMuseRelayMedia.Contribute?
    @State private var on = false
    @State private var loading = false
    @State private var busy = false
    @State private var message: String?
    @State private var confirmDelete = false
    @Environment(\.openURL) private var openURL

    var body: some View {
        Form {
            // C7: the conversations shared between the account's devices (NanoMuseSyncSettings.swift).
            NanoMuseSyncSection()
            if !NanoMuseCloud.isSignedIn {
                Section {
                    Text(AppLocalized("Sign in to morde Cloud to use it."))
                        .foregroundStyle(.secondary)
                }
            } else {
                Section {
                    Toggle(isOn: $on) {
                        VStack(alignment: .leading, spacing: 2) {
                            Text(AppLocalized("Help improve morde's AI models"))
                            if let info {
                                Text(String(format: AppLocalized("%d turns kept so far."), info.samples))
                                    .font(.footnote)
                                    .foregroundStyle(.secondary)
                            }
                        }
                    }
                    .disabled(busy || loading || info == nil)
                    .nmOnChange(of: on) { value in
                        guard let info, value != info.on, !busy else { return }
                        set(value)
                    }
                } footer: {
                    VStack(alignment: .leading, spacing: 8) {
                        if let record {
                            // the record first: the switch below decides only what joins the training set
                            Text(AppLocalized("morde Cloud keeps every turn of your chats with its models in full as your account's record: what went to the model, what it answered, the tools and what they returned, the pictures as files. From the 1.0.7 app on, turns answered on your own key join it too. It is your data: only your devices and whoever runs the relay can read it."))
                            Text(record.turns > 0
                                ? String(format: AppLocalized("The record holds %1$d turns of this account (%2$d run on your own keys), with %3$d pictures."), record.turns, record.own, record.files)
                                : AppLocalized("The record holds nothing of this account yet."))
                            Text(record.keepDays > 0
                                ? String(format: AppLocalized("This relay keeps a turn for %1$d days."), record.keepDays)
                                : AppLocalized("It stays until you delete it."))
                        }
                        Text(AppLocalized("While this is on, the text of those chats (what you wrote, what the model answered and the tools it chose to call) also joins the training set for the community's own open model: without your memory or SOUL (the system prompt), what tools returned, pictures, or who you are. Off, nothing more joins it; the record itself stays until you delete it. Turns on a key of your own are in the record, never in the training set."))
                        if let info {
                            Text(info.defaultOn ? AppLocalized("New accounts start with it on.") : AppLocalized("New accounts start with it off."))
                        }
                    }
                }

                if let info, !info.privacyURL.isEmpty, let url = URL(string: info.privacyURL) {
                    Section {
                        Button {
                            openURL(url)
                        } label: {
                            Label(AppLocalized("Privacy policy"), systemImage: "doc.text")
                        }
                    }
                }

                Section {
                    Button(role: .destructive) {
                        confirmDelete = true
                    } label: {
                        Label(deleteTitle, systemImage: "trash")
                    }
                    .disabled(busy || loading)
                } footer: {
                    Text(AppLocalized("Removes the record of this account's chats and every training turn kept from it, whether the switch is on or off now. Turning the switch off keeps what was kept until you delete it here."))
                }

                if let message {
                    Section {
                        Text(message).font(.footnote).foregroundStyle(.secondary)
                    }
                }
            }
        }
        .navigationTitle(AppLocalized("Data controls"))
        .navigationBarTitleDisplayMode(.inline)
        .overlay {
            if loading && info == nil {
                ProgressView()
            }
        }
        .confirmationDialog(deleteTitle, isPresented: $confirmDelete, titleVisibility: .visible) {
            Button(AppLocalized("Delete"), role: .destructive) { deleteSamples() }
            Button(AppLocalized("Cancel"), role: .cancel) {}
        } message: {
            Text(AppLocalized("The record of your chats and the kept training turns are removed from the server. This cannot be undone."))
        }
        .task { await load() }
    }

    /// The account's record on this relay, when it keeps one (relay 0.29).
    private var record: NanoMuseRelayMedia.Record? {
        info?.record.flatMap { $0.on ? $0 : nil }
    }

    /// With a record the delete removes it too; an older relay has only the training turns.
    private var deleteTitle: String {
        record != nil ? AppLocalized("Delete the record and the kept conversations") : AppLocalized("Delete the kept conversations")
    }

    private func load() async {
        guard NanoMuseCloud.isSignedIn else { return }
        loading = true
        defer { loading = false }
        do {
            let c = try await NanoMuseRelayMedia.contribute()
            info = c
            on = c.on
        } catch {
            message = NanoMuseCloud.describe(error)
        }
    }

    private func set(_ value: Bool) {
        busy = true
        Task { @MainActor in
            defer { busy = false }
            do {
                let c = try await NanoMuseRelayMedia.setContribute(value)
                info = c
                on = c.on
                message = c.on ? AppLocalized("On. Turn it off here at any time.") : AppLocalized("Off. Nothing more joins the training set.")
            } catch {
                on = info?.on ?? false
                message = NanoMuseCloud.describe(error)
            }
        }
    }

    private func deleteSamples() {
        busy = true
        Task { @MainActor in
            defer { busy = false }
            do {
                let gone = try await NanoMuseRelayMedia.deleteSamples()
                message = record != nil
                    ? String(format: AppLocalized("Deleted: %1$d turns of the record, %2$d files, %3$d training turns."), gone.turns, gone.files, gone.samples)
                    : String(format: AppLocalized("%d turns deleted."), gone.samples)
                if var c = info {
                    c.samples = 0
                    if var r = c.record {
                        r.turns = 0
                        r.own = 0
                        r.files = 0
                        r.mediaBytes = 0
                        c.record = r
                    }
                    info = c
                }
            } catch {
                message = NanoMuseCloud.describe(error)
            }
        }
    }
}
