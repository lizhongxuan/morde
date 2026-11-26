package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"morde/server/internal/domain"
)

type checkpointStoreStub struct {
	value   StoredAgentCheckpoint
	page    StoredAgentCheckpoint
	pending []AgentConversationMessage
	calls   int
}

type checkpointMessageSourceStoreStub struct {
	*checkpointStoreStub
	reference AgentMessageSourceReference
	page      AgentMessageSourcePageRead
}

func (s *checkpointMessageSourceStoreStub) ReadAgentMessageSourceReference(_ context.Context, _ AgentCheckpointFence,
	_ string) (AgentMessageSourceReference, error) {
	return s.reference, nil
}

func (s *checkpointMessageSourceStoreStub) ReadAgentMessageSourcePage(_ context.Context, _ AgentCheckpointFence,
	_ string, _ []byte, _ int64) (AgentMessageSourcePageRead, error) {
	return s.page, nil
}

func (s *checkpointStoreStub) SaveAgentCheckpoint(_ context.Context, value StoredAgentCheckpoint) (bool, error) {
	s.value = value
	s.calls++
	return false, nil
}

func (s *checkpointStoreStub) ReadAgentCheckpointPage(_ context.Context, _ AgentCheckpointPageRequest) (StoredAgentCheckpoint, error) {
	return s.page, nil
}

func (s *checkpointStoreStub) PendingMessages(_ context.Context, _ AgentCheckpointFence) ([]AgentConversationMessage, error) {
	return s.pending, nil
}

func checkpointTestCommand(payload string) AgentCheckpointCommand {
	digest := sha256.Sum256([]byte(payload))
	return AgentCheckpointCommand{
		Fence: AgentCheckpointFence{
			TaskID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567", AgentInstanceID: "agent_1", AgentGeneration: 1,
			AgentSessionID: "session_1", AuthorizationVersion: 1, LeaseOwner: "worker_1", LeaseVersion: 1,
			EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1",
		},
		From: 0, Through: 1, Digest: digest[:], Payload: []byte(payload),
	}
}

func TestAgentCheckpointEncryptedAndBoundToTask(t *testing.T) {
	store := &checkpointStoreStub{}
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	command := checkpointTestCommand(`{"header":{"id":"session_1"},"from_seq":0,"events":[{"seq":0,"text":"private content"},{"seq":1}]}`)
	if replayed, err := service.Save(context.Background(), command); err != nil || replayed {
		t.Fatalf("save checkpoint: replayed=%v err=%v", replayed, err)
	}
	if bytes.Contains(store.value.Ciphertext, []byte("private content")) {
		t.Fatal("checkpoint contains plaintext")
	}
	plain, err := service.Decode(store.value)
	if err != nil || !bytes.Equal(plain, command.Payload) {
		t.Fatalf("decode saved checkpoint failed: %v", err)
	}
	other := store.value
	other.Fence.TaskID = "q7654321"
	if _, err := service.Decode(other); err == nil {
		t.Fatal("checkpoint ciphertext accepted under another task")
	}
}

func TestAgentCheckpointChildIsBoundToRootSession(t *testing.T) {
	store := &checkpointStoreStub{}
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	command := checkpointTestCommand(`{"header":{"id":"session_1"},"from_seq":0,"events":[{"seq":0}]}`)
	command.Through = 0
	command.Fence.AgentRootSessionID = "session_root"
	digest := sha256.Sum256(command.Payload)
	command.Digest = digest[:]
	if _, err := service.Save(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	other := store.value
	other.Fence.AgentRootSessionID = "session_other"
	if _, err := service.Decode(other); err == nil {
		t.Fatal("child checkpoint ciphertext accepted under another root session")
	}
}

func TestAgentCheckpointRecognizesOnlyValidRootTurnStart(t *testing.T) {
	store := &checkpointStoreStub{}
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	command := checkpointTestCommand(`{"header":{"id":"session_1"},"from_seq":0,"events":[{"seq":0,"type":"turn/start","data":{"turn":1}}]}`)
	command.Through = 0
	if _, err := service.Save(context.Background(), command); err != nil || !store.value.RootTurnStarted {
		t.Fatalf("root turn start was not identified: value=%+v err=%v", store.value, err)
	}
	command.Fence.AgentRootSessionID = "session_root"
	if _, err := service.Save(context.Background(), command); err != nil || store.value.RootTurnStarted {
		t.Fatalf("child turn counted as root start: value=%+v err=%v", store.value, err)
	}
	command.Fence.AgentRootSessionID = "session_1"
	command.Payload = []byte(`{"header":{"id":"session_1"},"from_seq":0,"events":[{"seq":0,"type":"turn/start","data":{"turn":0}}]}`)
	digest := sha256.Sum256(command.Payload)
	command.Digest = digest[:]
	if _, err := service.Save(context.Background(), command); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("invalid turn start was accepted: %v", err)
	}
}

func TestAgentCheckpointRejectsBadDigestAndEventSequence(t *testing.T) {
	store := &checkpointStoreStub{}
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	command := checkpointTestCommand(`{"header":{"id":"session_1"},"from_seq":0,"events":[{"seq":0},{"seq":2}]}`)
	if _, err := service.Save(context.Background(), command); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("non-contiguous events accepted: %v", err)
	}
	command = checkpointTestCommand(`{"header":{"id":"session_1"},"from_seq":0,"events":[{"seq":0},{"seq":1}]}`)
	command.Digest[0] ^= 1
	if _, err := service.Save(context.Background(), command); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("wrong digest accepted: %v", err)
	}
	if store.calls != 0 {
		t.Fatalf("invalid checkpoints reached store %d times", store.calls)
	}
}

func TestAgentCheckpointReadPageDecodesAndPinsTail(t *testing.T) {
	store := &checkpointStoreStub{}
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	command := checkpointTestCommand(`{"header":{"id":"session_1"},"from_seq":0,"events":[{"seq":0},{"seq":1}]}`)
	if _, err := service.Save(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	store.page = store.value
	throughDigest := append([]byte(nil), command.Digest...)
	request := AgentCheckpointPageRequest{Fence: AgentInstanceFence{
		TaskID: command.Fence.TaskID, OwnerID: command.Fence.OwnerID, InitiatorID: command.Fence.InitiatorID, AgentInstanceID: "agent_2",
		AgentGeneration: 2, AgentSessionID: command.Fence.AgentSessionID, SupervisorID: "supervisor_1",
		LeaseOwner: "worker_2", LeaseVersion: 2, AuthorizationVersion: command.Fence.AuthorizationVersion,
		EngineVersion: command.Fence.EngineVersion, ProfileVersion: command.Fence.ProfileVersion,
	}, From: command.From, Through: command.Through, ThroughDigest: throughDigest}
	page, err := service.ReadPage(context.Background(), request)
	if err != nil || page.From != command.From || page.Through != command.Through ||
		!bytes.Equal(page.Payload, command.Payload) || !bytes.Equal(page.Digest, command.Digest) {
		t.Fatalf("read checkpoint page = %#v, %v", page, err)
	}
	request.ThroughDigest[0] ^= 1
	if _, err := service.ReadPage(context.Background(), request); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("changed checkpoint tail accepted: %v", err)
	}
}

func TestAgentCheckpointExtractsUserMessageText(t *testing.T) {
	store := &checkpointStoreStub{}
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	command := checkpointTestCommand(`{"header":{"id":"session_1"},"from_seq":0,"events":[{"seq":0,"type":"user/message","data":{"id":"m1234567","role":"user","content":[{"type":"text","text":"hello "},{"type":"image","url":"private"},{"type":"text","text":"world"}]}}]}`)
	command.Through = 0
	digest := sha256.Sum256(command.Payload)
	command.Digest = digest[:]
	if _, err := service.Save(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	if store.value.ConversationMessagesInvalid || len(store.value.ConversationMessages) != 1 ||
		store.value.ConversationMessages[0].ID != "m1234567" || store.value.ConversationMessages[0].Content != "hello world" ||
		len(store.value.ConversationMessages[0].AttachmentIDs) != 0 {
		t.Fatalf("checkpoint message mapping = %#v invalid=%t", store.value.ConversationMessages, store.value.ConversationMessagesInvalid)
	}

	command.Payload = []byte(`{"header":{"id":"session_1"},"from_seq":0,"events":[{"seq":0,"type":"user/message","data":{"id":"m1234567","role":"user","content":null}}]}`)
	digest = sha256.Sum256(command.Payload)
	command.Digest = digest[:]
	if _, err := service.Save(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	if !store.value.ConversationMessagesInvalid || len(store.value.ConversationMessages) != 0 {
		t.Fatalf("malformed user message was not marked invalid: %#v", store.value)
	}

	command.Payload = []byte(`{"header":{"id":"session_1"},"from_seq":0,"events":[{"seq":0,"type":"user/message","data":{"id":"2d47aa7b-f4db-4f11-b606-b7102c2c2df2","role":"user","content":"task prompt"}}]}`)
	digest = sha256.Sum256(command.Payload)
	command.Digest = digest[:]
	if _, err := service.Save(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	if store.value.ConversationMessagesInvalid || len(store.value.ConversationMessages) != 0 {
		t.Fatalf("dsh-generated prompt was not ignored: %#v", store.value)
	}

	command.Payload = []byte(`{"header":{"id":"session_1"},"from_seq":0,"events":[{"seq":0,"type":"user/message","data":{"id":"invalid","role":"user","content":"hello"}}]}`)
	digest = sha256.Sum256(command.Payload)
	command.Digest = digest[:]
	if _, err := service.Save(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	if !store.value.ConversationMessagesInvalid {
		t.Fatal("malformed non-UUID message ID was not marked invalid")
	}
}

func TestAgentCheckpointTreatsBareReferencePrefixAsUserText(t *testing.T) {
	payload := []byte(`{"events":[{"seq":0,"type":"user/message","data":{"id":"m1234567","role":"user","content":"MORDE_SOURCE_REFERENCE_V1"}}]}`)
	messages, invalid := checkpointConversationMessages(payload)
	if invalid || len(messages) != 1 || messages[0].Content != "MORDE_SOURCE_REFERENCE_V1" || messages[0].Reference != nil {
		t.Fatalf("bare reference prefix was not preserved as user text: messages=%+v invalid=%t", messages, invalid)
	}
	markerLike := []byte(`{"events":[{"seq":0,"type":"user/message","data":{"id":"m1234567","role":"user","content":"MORDE_SOURCE_REFERENCE_V1 message_id=m1234567 source_sha256=0000000000000000000000000000000000000000000000000000000000000000 content_bytes=1 manifest_sha256=0000000000000000000000000000000000000000000000000000000000000000 representation_sha256=0000000000000000000000000000000000000000000000000000000000000000\ninvalid"}}]}`)
	if _, invalid := checkpointConversationMessages(markerLike); !invalid {
		t.Fatal("complete reference header with invalid payload was accepted")
	}
}

func TestAgentCheckpointExtractsAndValidatesQueuedAttachmentIDs(t *testing.T) {
	store := &checkpointStoreStub{}
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	first := "pa_0123456789abcdef0123456789abcdef"
	second := "pa_1123456789abcdef0123456789abcdef"
	makeCommand := func(marker string) AgentCheckpointCommand {
		payload := `{"header":{"id":"session_1"},"from_seq":0,"events":[{"seq":0,"type":"user/message","data":{"id":"m1234567","role":"user","content":[{"type":"text","text":"please inspect"},{"type":"text","text":"` + marker + `"}]}}]}`
		command := checkpointTestCommand(payload)
		command.Through = 0
		digest := sha256.Sum256(command.Payload)
		command.Digest = digest[:]
		return command
	}
	command := makeCommand(checkpointAttachmentMarkerPrefix + first + "," + second + "]")
	if _, err := service.Save(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	message := store.value.ConversationMessages[0]
	if store.value.ConversationMessagesInvalid || message.Content != "please inspect" ||
		!sameCheckpointAttachmentIDs(message.AttachmentIDs, []string{first, second}) {
		t.Fatalf("queued attachment checkpoint parsed incorrectly: %#v invalid=%t", message, store.value.ConversationMessagesInvalid)
	}
	projectID := "pca_0123456789abcdef0123456789abcdef"
	if _, err := service.Save(context.Background(), makeCommand(checkpointAttachmentMarkerPrefix+projectID+"]")); err != nil ||
		store.value.ConversationMessagesInvalid || len(store.value.ConversationMessages) != 1 ||
		!sameCheckpointAttachmentIDs(store.value.ConversationMessages[0].AttachmentIDs, []string{projectID}) {
		t.Fatalf("project attachment marker was not recognized: %#v err=%v", store.value, err)
	}
	for _, marker := range []string{
		checkpointAttachmentMarkerPrefix + second + "," + first + "]",
		checkpointAttachmentMarkerPrefix + first + "," + first + "]",
		checkpointAttachmentMarkerPrefix + "not-an-attachment]",
		checkpointAttachmentMarkerPrefix + "pca_0123456789abcdef0123456789abcdeF]",
		checkpointAttachmentMarkerPrefix + first,
	} {
		if _, err := service.Save(context.Background(), makeCommand(marker)); err != nil {
			t.Fatal(err)
		}
		if !store.value.ConversationMessagesInvalid || len(store.value.ConversationMessages) != 0 {
			t.Fatalf("invalid queued attachment marker was accepted: %q %#v", marker, store.value)
		}
	}
}

func TestAgentCheckpointAllowsAttachmentOnlyMessage(t *testing.T) {
	const id = "pa_0123456789abcdef0123456789abcdef"
	payload := []byte(`{"events":[{"seq":0,"type":"user/message","data":{"id":"m1234567","role":"user","content":[{"type":"text","text":""},{"type":"text","text":"[MORDE_TASK_ATTACHMENT_IDS_V1:` + id + `]"}]}}]}`)
	messages, invalid := checkpointConversationMessages(payload)
	if invalid || len(messages) != 1 || messages[0].Content != "" || !sameCheckpointAttachmentIDs(messages[0].AttachmentIDs, []string{id}) {
		t.Fatalf("attachment-only message was not mapped: messages=%+v invalid=%t", messages, invalid)
	}
	empty := []byte(`{"events":[{"seq":0,"type":"user/message","data":{"id":"m1234567","role":"user","content":[{"type":"text","text":""}]}}]}`)
	if _, invalid := checkpointConversationMessages(empty); !invalid {
		t.Fatal("empty message without attachments was accepted")
	}
}

func TestAgentMessageEmptySourceRequiresCanonicalManifestAndAttachment(t *testing.T) {
	reference := checkpointTestCanonicalEmptyMessageSource(t)
	if err := ValidateAgentMessageReferenceEnvelope(reference.Envelope); err != nil {
		t.Fatalf("canonical empty source envelope was rejected: %v", err)
	}
	if err := ValidateAgentMessageSourceReference(reference); err != nil {
		t.Fatalf("canonical empty source receipt was rejected: %v", err)
	}
	marker, err := FormatAgentMessageReferenceEnvelope(reference.Envelope)
	if err != nil {
		t.Fatalf("format canonical empty source reference: %v", err)
	}
	parsed, err := ParseAgentMessageReferenceEnvelope(marker)
	if err != nil || parsed.ContentBytes != 0 || parsed.TotalRunes != 0 || parsed.Pages == nil || len(parsed.Pages) != 0 ||
		!sameCheckpointAttachmentIDs(parsed.AttachmentIDs, reference.Envelope.AttachmentIDs) {
		t.Fatalf("canonical empty source reference round-trip changed: %+v err=%v", parsed, err)
	}

	fence := checkpointTestCommand(`{"header":{"id":"session_1"},"from_seq":0,"events":[{"seq":0}]}`).Fence
	store := &checkpointMessageSourceStoreStub{checkpointStoreStub: &checkpointStoreStub{}, reference: reference}
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	read, err := service.ReadMessageSourceReference(context.Background(), fence, reference.Envelope.MessageID)
	if err != nil || read.Envelope.ContentBytes != 0 || read.Envelope.TotalRunes != 0 || len(read.Envelope.Pages) != 0 {
		t.Fatalf("checkpoint source reference read = %+v, %v", read, err)
	}

	unattached := reference.Envelope
	unattached.AttachmentIDs = []string{}
	if err := ValidateAgentMessageReferenceEnvelope(unattached); err == nil {
		t.Fatal("empty source without an attachment was accepted")
	}
	unattached = reference.Envelope
	unattached.AttachmentIDs = nil
	if err := ValidateAgentMessageReferenceEnvelope(unattached); err == nil {
		t.Fatal("empty source with a nil attachment list was accepted")
	}
}

func TestAgentMessageEmptySourceRejectsNoncanonicalManifest(t *testing.T) {
	base := checkpointTestCanonicalEmptyMessageSource(t).Envelope
	emptyDigest := sha256.Sum256(nil)
	badDigest := sha256.Sum256([]byte("not empty"))
	withPage := []AgentMessageSourcePage{{RuneOffset: 0, NextRuneOffset: 1, SHA256: hex.EncodeToString(emptyDigest[:])}}
	tests := []struct {
		name   string
		mutate func(*AgentMessageReferenceEnvelope)
	}{
		{name: "nonempty source digest", mutate: func(value *AgentMessageReferenceEnvelope) {
			value.SourceSHA256 = hex.EncodeToString(badDigest[:])
		}},
		{name: "nonzero content bytes", mutate: func(value *AgentMessageReferenceEnvelope) { value.ContentBytes = 1 }},
		{name: "nonzero total runes", mutate: func(value *AgentMessageReferenceEnvelope) { value.TotalRunes = 1 }},
		{name: "nonempty pages", mutate: func(value *AgentMessageReferenceEnvelope) { value.Pages = withPage }},
		{name: "nil pages", mutate: func(value *AgentMessageReferenceEnvelope) { value.Pages = nil }},
		{name: "unsorted attachment IDs", mutate: func(value *AgentMessageReferenceEnvelope) {
			value.AttachmentIDs = []string{"pa_1123456789abcdef0123456789abcdef", "pa_0123456789abcdef0123456789abcdef"}
		}},
		{name: "invalid attachment ID", mutate: func(value *AgentMessageReferenceEnvelope) {
			value.AttachmentIDs = []string{"not-an-attachment"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := base
			value.AttachmentIDs = append([]string(nil), base.AttachmentIDs...)
			value.Pages = append([]AgentMessageSourcePage{}, base.Pages...)
			test.mutate(&value)
			if err := ValidateAgentMessageReferenceEnvelope(value); err == nil {
				t.Fatal("noncanonical empty source reference was accepted")
			}
		})
	}
	if _, err := ComputeAgentMessageManifestSHA256(base.MessageID, base.MessageSequence, 0, badDigest[:], 0, []AgentMessageSourcePage{}); err == nil {
		t.Fatal("empty manifest accepted a digest other than SHA256(empty)")
	}
	if _, err := ComputeAgentMessageManifestSHA256(base.MessageID, base.MessageSequence, 0, emptyDigest[:], 0, nil); err == nil {
		t.Fatal("empty manifest accepted nil pages instead of canonical []")
	}

	content := []byte("ordinary message")
	contentDigest := sha256.Sum256(content)
	pageDigest := sha256.Sum256(content)
	manifest, err := ComputeAgentMessageManifestSHA256("m1234567", 1, int64(len(content)), contentDigest[:], int64(len(content)),
		[]AgentMessageSourcePage{{RuneOffset: 0, NextRuneOffset: int64(len(content)), SHA256: hex.EncodeToString(pageDigest[:])}})
	if err != nil || len(manifest) != sha256.Size {
		t.Fatalf("ordinary nonempty source manifest changed: digest=%x err=%v", manifest, err)
	}
}

func TestAgentMessageSourcePageRejectsEmptySource(t *testing.T) {
	reference := checkpointTestCanonicalEmptyMessageSource(t)
	emptyDigest := sha256.Sum256(nil)
	store := &checkpointMessageSourceStoreStub{checkpointStoreStub: &checkpointStoreStub{}, reference: reference,
		page: AgentMessageSourcePageRead{MessageID: reference.Envelope.MessageID, SourceSHA256: append([]byte(nil), emptyDigest[:]...),
			PageSHA256: append([]byte(nil), emptyDigest[:]...), FragmentSHA256: append([]byte(nil), emptyDigest[:]...),
			PageRuneOffset: 0, PageNextRuneOffset: 0, RuneOffset: 0, NextRuneOffset: 0, TotalRunes: 0, Content: ""}}
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	fence := checkpointTestCommand(`{"header":{"id":"session_1"},"from_seq":0,"events":[{"seq":0}]}`).Fence
	if _, err := service.ReadMessageSourcePage(context.Background(), fence, reference.Envelope.MessageID, emptyDigest[:], 0); err == nil {
		t.Fatal("empty source returned a readable source page")
	}
}

func checkpointTestCanonicalEmptyMessageSource(t *testing.T) AgentMessageSourceReference {
	t.Helper()
	emptyDigest := sha256.Sum256(nil)
	envelope := AgentMessageReferenceEnvelope{Version: AgentMessageReferenceEnvelopeVersion,
		TaskID: "q1234567", TaskRevision: 0, OwnerID: "u1234567", ConversationID: "c1234567",
		MessageID: "m1234567", MessageSequence: 1, AuthorizationVersion: 1, AgentGeneration: 1,
		RootSessionID: "session_1", Mode: AgentMessageDeliveryFollowup,
		AttachmentIDs: []string{"pa_0123456789abcdef0123456789abcdef"}, ContentBytes: 0,
		SourceSHA256: hex.EncodeToString(emptyDigest[:]), ManifestVersion: AgentMessageManifestVersion,
		TotalRunes: 0, Pages: []AgentMessageSourcePage{}}
	manifestSHA256, err := ComputeAgentMessageManifestSHA256(envelope.MessageID, envelope.MessageSequence,
		envelope.ContentBytes, emptyDigest[:], envelope.TotalRunes, envelope.Pages)
	if err != nil {
		t.Fatalf("compute canonical empty manifest: %v", err)
	}
	envelope.ManifestSHA256 = hex.EncodeToString(manifestSHA256)
	representationSHA256, err := ComputeAgentMessageReferenceRepresentationSHA256(envelope)
	if err != nil {
		t.Fatalf("compute canonical empty representation: %v", err)
	}
	envelope.RepresentationSHA256 = hex.EncodeToString(representationSHA256)
	return AgentMessageSourceReference{Envelope: envelope, RepresentationKind: "reference", RepresentationVersion: 1,
		RepresentationSHA256: append([]byte(nil), representationSHA256...), AppliedCheckpointThrough: 0, AuthorizedAgentGeneration: 1}
}

func TestAgentCheckpointPendingMessagesRequiresRootFence(t *testing.T) {
	store := &checkpointStoreStub{pending: []AgentConversationMessage{{ID: "m1234567", Content: "queued"}}}
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	fence := checkpointTestCommand(`{"header":{"id":"session_1"},"from_seq":0,"events":[{"seq":0}]}`).Fence
	messages, err := service.PendingMessages(context.Background(), fence)
	if err != nil || len(messages) != 1 || messages[0].ID != "m1234567" {
		t.Fatalf("pending messages = %#v, %v", messages, err)
	}
	fence.AgentRootSessionID = "session_root"
	if _, err := service.PendingMessages(context.Background(), fence); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("child session requested root pending messages: %v", err)
	}
}
