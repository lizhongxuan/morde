package app

import "testing"

func TestCheckpointInboxSplicesBindModeToMessage(t *testing.T) {
	payload := []byte(`{"events":[
{"seq":4,"type":"agent/inbox/spliced","data":{"target":"next-step","start":0,"inserted":[{"id":"m1234567","role":"user","content":"补充要求","source":{"kind":"user"}}]}},
{"seq":5,"type":"agent/inbox/spliced","data":{"target":"next-turn","start":0,"inserted":[{"id":"m2345678","role":"user","content":[{"type":"text","text":"follow up"},{"type":"text","text":"[MORDE_TASK_ATTACHMENT_IDS_V1:pa_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa]"}],"source":{"kind":"user"}}]}}
]}`)
	splices, invalid := checkpointInboxSplices(payload)
	if invalid || len(splices) != 2 || splices[0].Sequence != 4 || splices[0].Target != "next-step" ||
		len(splices[0].Messages) != 1 || splices[0].Messages[0].ID != "m1234567" ||
		splices[0].Messages[0].Mode != AgentMessageDeliverySteer || splices[0].Messages[0].Content != "补充要求" ||
		splices[1].Sequence != 5 || splices[1].Messages[0].Mode != AgentMessageDeliveryFollowup ||
		splices[1].Messages[0].Content != "follow up" || len(splices[1].Messages[0].AttachmentIDs) != 1 {
		t.Fatalf("inbox delivery evidence = %#v, invalid=%t", splices, invalid)
	}
}

func TestCheckpointInboxSplicesRejectInvalidServerMessage(t *testing.T) {
	for _, payload := range []string{
		`{"events":[{"seq":1,"type":"agent/inbox/spliced","data":{"target":"later","start":0,"inserted":[]}}]}`,
		`{"events":[{"type":"agent/inbox/spliced","data":{"target":"next-step","start":0,"inserted":[]}}]}`,
		`{"events":[{"seq":1,"type":"agent/inbox/spliced","data":{"target":"next-step","start":0,"inserted":[{"id":"m1234567","role":"assistant","content":"bad","source":{"kind":"user"}}]}}]}`,
	} {
		if _, invalid := checkpointInboxSplices([]byte(payload)); !invalid {
			t.Fatalf("accepted invalid inbox splice: %s", payload)
		}
	}
}
