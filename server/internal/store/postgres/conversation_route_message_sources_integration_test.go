package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
)

func conversationRouteMessageSourcesFenceError(ctx context.Context, f routeActionsTestFixture,
	claim app.ConversationRouteClaim, value app.ConversationRouteContext) error {
	return f.store.WithTx(ctx, func(tx pgx.Tx) error {
		return checkConversationRouteModelFence(ctx, tx, app.ConversationRouteModelFence{Claim: claim, Context: value}, "p1234567")
	})
}

func projectedConversationRouteMessageSources(value app.ConversationRouteContext, previewBytes int) app.ConversationRouteContext {
	value.ContentSHA256 = append([]byte(nil), value.ContentSHA256...)
	value.ContentSources = append([]app.ConversationRouteMessageSource(nil), value.ContentSources...)
	for index := range value.ContentSources {
		value.ContentSources[index].ContentSHA256 = append([]byte(nil), value.ContentSources[index].ContentSHA256...)
	}
	app.ProjectConversationRouteMessage(&value, previewBytes)
	return value
}

func TestConversationRouteJoinedMessageSourcesFenceAndPreviews(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	rootText := strings.Repeat("root requirement ", 82)
	supplementText := strings.Repeat("supplement requirement ", 36)
	root := f.receive(t, "route-source-manifest-root", rootText, app.ConversationRoutingInput{})
	rootSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	rootSnapshot.Context.ModelID = "p1234567"
	child := f.receive(t, "route-source-manifest-child", supplementText, app.ConversationRoutingInput{})
	childSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, child)
	if err != nil || !app.ValidConversationRouteJoinTarget(root.Message.ID, childSnapshot.Context.RequestCandidates) {
		t.Fatalf("root was not offered as the same-scope join target: candidates=%+v err=%v",
			childSnapshot.Context.RequestCandidates, err)
	}
	childSnapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, child, childSnapshot,
		app.ConversationRouteDecision{Action: app.ConversationRouteJoin, TargetMessageID: root.Message.ID}); err != nil {
		t.Fatalf("join supplement to the root: %v", err)
	}
	if err := f.store.ApplyConversationRoute(ctx, root, rootSnapshot,
		app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatalf("reclassify the root after a joined supplement: %v", err)
	}
	reclaimedRoot, found, err := f.store.ClaimConversationRoute(ctx, "route-source-manifest-finalize", time.Minute)
	if err != nil || !found || reclaimedRoot.Message.ID != root.Message.ID {
		t.Fatalf("reclaim aggregation root: claim=%+v found=%t err=%v", reclaimedRoot, found, err)
	}

	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, reclaimedRoot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if len(snapshot.Context.Content) <= app.ConversationRouteCurrentMessagePreviewBytes ||
		!strings.Contains(snapshot.Context.Content, rootText) || !strings.Contains(snapshot.Context.Content, supplementText) {
		t.Fatalf("joined route source is not a complete >2KiB aggregate: bytes=%d", len(snapshot.Context.Content))
	}
	if len(snapshot.Context.ContentSources) != 2 || snapshot.Context.ContentSources[0].MessageID != root.Message.ID ||
		snapshot.Context.ContentSources[0].Sequence != root.Message.Sequence ||
		snapshot.Context.ContentSources[1].MessageID != child.Message.ID ||
		snapshot.Context.ContentSources[1].Sequence != child.Message.Sequence {
		t.Fatalf("joined source manifest does not preserve root/supplement order: %+v", snapshot.Context.ContentSources)
	}
	for index, source := range snapshot.Context.ContentSources {
		content := []string{rootText, supplementText}[index]
		digest := sha256.Sum256([]byte(content))
		if source.ContentBytes != int64(len(content)) || string(source.ContentSHA256) != string(digest[:]) {
			t.Fatalf("source %d digest/length does not bind its original message: %+v", index, source)
		}
	}

	fullContent := snapshot.Context.Content
	contentDigest := sha256.Sum256([]byte(fullContent))
	for _, previewBytes := range []int{app.ConversationRouteCurrentMessagePreviewBytes, 1024} {
		value := projectedConversationRouteMessageSources(snapshot.Context, previewBytes)
		wantPreview, truncated := app.ConversationRouteTextPreview(fullContent, previewBytes)
		if !truncated || value.Content != wantPreview || !value.ContentTruncated ||
			value.ContentBytes != int64(len(fullContent)) || string(value.ContentSHA256) != string(contentDigest[:]) {
			t.Fatalf("%d-byte preview lost the aggregate source proof: bytes=%d context=%+v", previewBytes, len(value.Content), value)
		}
		request, err := app.ConversationRouteModelRequest(value)
		if err != nil {
			t.Fatalf("build %d-byte aggregate route request: %v", previewBytes, err)
		}
		var input struct {
			Message          string `json:"message"`
			MessageBytes     int64  `json:"message_bytes"`
			MessageSHA256    string `json:"message_sha256"`
			MessageTruncated bool   `json:"message_truncated"`
			MessageSources   []struct {
				MessageID    string `json:"message_id"`
				Sequence     int64  `json:"sequence"`
				ContentBytes int64  `json:"content_bytes"`
				SHA256       string `json:"sha256"`
			} `json:"message_sources"`
		}
		if err := json.Unmarshal([]byte(request.Input[0].Text), &input); err != nil {
			t.Fatal(err)
		}
		if input.Message != wantPreview || input.MessageBytes != int64(len(fullContent)) ||
			input.MessageSHA256 != fmt.Sprintf("%x", contentDigest) || !input.MessageTruncated || len(input.MessageSources) != 2 ||
			input.MessageSources[0].MessageID != root.Message.ID || input.MessageSources[1].MessageID != child.Message.ID {
			t.Fatalf("model request lost ordered source proofs: %+v", input)
		}
		for index, source := range snapshot.Context.ContentSources {
			if input.MessageSources[index].Sequence != source.Sequence || input.MessageSources[index].ContentBytes != source.ContentBytes ||
				input.MessageSources[index].SHA256 != fmt.Sprintf("%x", source.ContentSHA256) {
				t.Fatalf("model request lost source %d sequence/size/digest: %+v", index, input.MessageSources[index])
			}
		}
		if err := conversationRouteMessageSourcesFenceError(ctx, f, reclaimedRoot, value); err != nil {
			t.Fatalf("valid aggregate projection was rejected by the fresh database fence (%d bytes): %v", previewBytes, err)
		}
	}

	assertRejected := func(name string, mutate func(*app.ConversationRouteContext)) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			value := projectedConversationRouteMessageSources(snapshot.Context, app.ConversationRouteCurrentMessagePreviewBytes)
			mutate(&value)
			if err := conversationRouteMessageSourcesFenceError(ctx, f, reclaimedRoot, value); err == nil {
				t.Fatal("tampered aggregate source proof was accepted")
			}
		})
	}
	assertRejected("changed aggregate digest", func(value *app.ConversationRouteContext) { value.ContentSHA256[0] ^= 0xff })
	assertRejected("changed source member", func(value *app.ConversationRouteContext) {
		value.ContentSources[1].MessageID = "m0000000"
	})
	assertRejected("reordered source members", func(value *app.ConversationRouteContext) {
		value.ContentSources[0], value.ContentSources[1] = value.ContentSources[1], value.ContentSources[0]
	})
	assertRejected("changed member digest", func(value *app.ConversationRouteContext) { value.ContentSources[1].ContentSHA256[0] ^= 0xff })

	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_message_routes
SET authorization_snapshot=jsonb_set(authorization_snapshot,'{Version}','2'::jsonb)
WHERE message_id=$1`, child.Message.ID); err != nil {
		t.Fatal(err)
	}
	if err := conversationRouteMessageSourcesFenceError(ctx, f, reclaimedRoot,
		projectedConversationRouteMessageSources(snapshot.Context, app.ConversationRouteCurrentMessagePreviewBytes)); err == nil {
		t.Fatal("source member authorization change was accepted")
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_message_routes
SET authorization_snapshot=jsonb_set(authorization_snapshot,'{Version}','1'::jsonb),
 request_metadata=jsonb_set(request_metadata,'{host_id}','"h1234567"'::jsonb,true)
WHERE message_id=$1`, child.Message.ID); err != nil {
		t.Fatal(err)
	}
	if err := conversationRouteMessageSourcesFenceError(ctx, f, reclaimedRoot,
		projectedConversationRouteMessageSources(snapshot.Context, app.ConversationRouteCurrentMessagePreviewBytes)); err == nil {
		t.Fatal("source member execution scope change was accepted")
	}
}

func TestConversationRouteJoinedMessageSourcesDetachOversizedSupplement(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	rootText := "ROOT-OVERSIZED-AGGREGATE\n" + strings.Repeat("r", 17000)
	childText := "DETACHED-OVERSIZED-SUPPLEMENT\n" + strings.Repeat("s", 17000)
	root := f.receive(t, "route-source-limit-root", rootText, app.ConversationRoutingInput{})
	rootSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	rootSnapshot.Context.ModelID = "p1234567"
	child := f.receive(t, "route-source-limit-child", childText, app.ConversationRoutingInput{})
	childSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, child)
	if err != nil || !app.ValidConversationRouteJoinTarget(root.Message.ID, childSnapshot.Context.RequestCandidates) {
		t.Fatalf("large same-scope supplement is not joinable: snapshot=%+v err=%v", childSnapshot.Context, err)
	}
	childSnapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, child, childSnapshot,
		app.ConversationRouteDecision{Action: app.ConversationRouteJoin, TargetMessageID: root.Message.ID}); err != nil {
		t.Fatalf("join large supplement to root: %v", err)
	}
	if err := f.store.ApplyConversationRoute(ctx, root, rootSnapshot,
		app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatalf("oversized supplement should not fail the root route: %v", err)
	}
	var taskCount int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE conversation_id=$1`, f.conversation.ID).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if taskCount != 1 {
		t.Fatalf("root route created %d Tasks, want 1", taskCount)
	}
	var objective string
	if err := f.store.pool.QueryRow(ctx, `SELECT objective FROM tasks WHERE conversation_id=$1`, f.conversation.ID).Scan(&objective); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(objective, "message_id="+root.Message.ID) || !strings.Contains(objective, "ROOT-OVERSIZED-AGGREGATE") ||
		strings.Contains(objective, "DETACHED-OVERSIZED-SUPPLEMENT") {
		t.Fatalf("root Task objective lost its long-message source or included the detached supplement: %s", objective)
	}
	var duplicateOf *string
	var routeState, deliveryStatus string
	if err := f.store.pool.QueryRow(ctx, `SELECT r.duplicate_of_message_id,r.state,m.delivery_status
FROM conversation_message_routes r JOIN conversation_messages m ON m.id=r.message_id WHERE r.message_id=$1`, child.Message.ID).
		Scan(&duplicateOf, &routeState, &deliveryStatus); err != nil {
		t.Fatal(err)
	}
	if duplicateOf != nil || routeState != "pending" || deliveryStatus != "pending" {
		t.Fatalf("oversized supplement was not independently requeued: duplicate=%v route=%s delivery=%s",
			duplicateOf, routeState, deliveryStatus)
	}
}

func TestConversationRouteApplyKeepsLongRootAndSelectedSupplementInTaskObjective(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	rootText := "ROOT-SOURCE-ANCHOR\n" + strings.Repeat("root requirement ", 320)
	supplementText := "SUPPLEMENT-SOURCE-ANCHOR\n" + strings.Repeat("additional requirement ", 180)
	root := f.receive(t, "route-source-objective-root", rootText, app.ConversationRoutingInput{})
	rootSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	rootSnapshot.Context.ModelID = "p1234567"
	if started, err := f.store.BeginConversationRouteAggregation(ctx, root, rootSnapshot,
		app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil || !started {
		t.Fatalf("begin fixed aggregation window: started=%t err=%v", started, err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_message_routes
SET aggregation_deadline=clock_timestamp()+interval '10 seconds' WHERE message_id=$1`, root.Message.ID); err != nil {
		t.Fatal(err)
	}
	child := f.receive(t, "route-source-objective-child", supplementText, app.ConversationRoutingInput{})
	childSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, child)
	if err != nil || !app.ValidConversationRouteJoinTarget(root.Message.ID, childSnapshot.Context.RequestCandidates) {
		t.Fatalf("long supplement could not identify its waiting root: candidates=%+v err=%v",
			childSnapshot.Context.RequestCandidates, err)
	}
	childSnapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, child, childSnapshot,
		app.ConversationRouteDecision{Action: app.ConversationRouteJoin, TargetMessageID: root.Message.ID}); err != nil {
		t.Fatalf("link supplement to waiting root: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_message_routes
SET aggregation_deadline=clock_timestamp()+interval '5 milliseconds' WHERE message_id=$1`, root.Message.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	finalRoot, found, err := f.store.ClaimConversationRoute(ctx, "route-source-objective-finalize", time.Minute)
	if err != nil || !found || finalRoot.Message.ID != root.Message.ID {
		t.Fatalf("claim waiting root after deadline: claim=%+v found=%t err=%v", finalRoot, found, err)
	}
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, finalRoot)
	if err != nil || len(snapshot.Context.AggregationCandidates) != 1 ||
		snapshot.Context.AggregationCandidates[0].MessageID != child.Message.ID {
		t.Fatalf("load the selected supplement candidate: candidates=%+v err=%v", snapshot.Context.AggregationCandidates, err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, finalRoot, snapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteNew, AggregateMessageIDs: []string{child.Message.ID},
	}); err != nil {
		t.Fatalf("create Task with the selected supplement: %v", err)
	}
	var objective string
	if err := f.store.pool.QueryRow(ctx, `SELECT objective FROM tasks WHERE conversation_id=$1`, f.conversation.ID).Scan(&objective); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(objective, rootText) || !strings.Contains(objective, supplementText) ||
		strings.Index(objective, rootText) > strings.Index(objective, supplementText) {
		t.Fatalf("Task objective dropped or reordered a selected source: bytes=%d rootBytes=%d supplementBytes=%d",
			len(objective), len(rootText), len(supplementText))
	}
}
