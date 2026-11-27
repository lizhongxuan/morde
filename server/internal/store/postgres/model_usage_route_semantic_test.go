package postgres

import (
	"testing"

	"morde/server/internal/app"
)

func TestValidStoredRouteDecisionAcceptsStructuredExplicitNewWithoutLexiconMatch(t *testing.T) {
	context := app.ConversationRouteContext{
		Content: "帮我检查一下接口的输入校验，并补上测试。",
	}
	decision := app.ConversationRouteDecision{
		Action:      app.ConversationRouteNew,
		ExplicitNew: true,
	}
	if !validStoredRouteDecision(decision, context) {
		t.Fatal("stored structured explicit-new decision was rejected without a matching phrase")
	}
}

func TestValidStoredRouteDecisionKeepsLongReplyFieldsBehindExplicitLongMode(t *testing.T) {
	context := app.ConversationRouteContext{Content: "Explain this carefully"}
	for _, decision := range []app.ConversationRouteDecision{
		{Action: app.ConversationRouteReply, Reply: "A long answer.", LongReply: true},
		{Action: app.ConversationRouteReply, Reply: "A partial answer.", LongReply: true, Partial: true},
		{Action: app.ConversationRouteClarify, Question: "Please narrow the question.", LongReply: true},
	} {
		if !validStoredRouteDecision(decision, context, true) {
			t.Fatalf("valid explicit long-form decision was rejected: %+v", decision)
		}
		if validStoredRouteDecision(decision, context) {
			t.Fatalf("long-form decision was accepted on the default short path: %+v", decision)
		}
	}
	for _, decision := range []app.ConversationRouteDecision{
		{Action: app.ConversationRouteReply, Reply: "A long answer."},
		{Action: app.ConversationRouteReply, Reply: "A partial answer.", LongReply: true, Cancelled: true},
		{Action: app.ConversationRouteNew, LongReply: true},
		{Action: app.ConversationRouteClarify, Question: "Please narrow the question.", LongReply: true, Partial: true},
	} {
		if validStoredRouteDecision(decision, context, true) {
			t.Fatalf("invalid long-form decision was accepted: %+v", decision)
		}
	}
}

func TestValidStoredRouteDecisionAcceptsBoundedDirectReadOnlyTools(t *testing.T) {
	context := app.ConversationRouteContext{Content: "查找这个主题的公开资料"}
	for _, test := range []struct {
		name      string
		toolName  string
		arguments string
	}{
		{name: "web search", toolName: "morde_web_search", arguments: `{"query":"日本央行利率政策"}`},
		{name: "web read", toolName: "morde_web_read", arguments: `{"url":"https://example.com/article"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := app.ConversationRouteDecision{
				Action: app.ConversationRouteTool, ToolName: test.toolName, ToolArguments: test.arguments,
				ModelOutput: `{"action":"tool","tool_name":"` + test.toolName + `","tool_arguments":` + test.arguments + `}`,
			}
			if !validStoredRouteDecision(decision, context) {
				t.Fatalf("valid bounded direct tool decision was rejected: %+v", decision)
			}
		})
	}
}

func TestValidStoredRouteDecisionRejectsInvalidDirectToolFieldsAndContext(t *testing.T) {
	context := app.ConversationRouteContext{Content: "查找这个主题的公开资料"}
	valid := app.ConversationRouteDecision{
		Action: app.ConversationRouteTool, ToolName: "morde_web_search", ToolArguments: `{"query":"公开资料"}`,
		ModelOutput: `{"action":"tool","tool_name":"morde_web_search","tool_arguments":{"query":"公开资料"}}`,
	}
	invalidDecisions := []app.ConversationRouteDecision{
		{Action: app.ConversationRouteTool, ToolName: "morde_personal_update", ToolArguments: valid.ToolArguments, ModelOutput: valid.ModelOutput},
		{Action: app.ConversationRouteTool, ToolName: valid.ToolName, ToolArguments: `{"query":"公开资料","unexpected":true}`, ModelOutput: valid.ModelOutput},
		{Action: app.ConversationRouteTool, ToolName: valid.ToolName, ToolArguments: valid.ToolArguments, TaskID: "q1234567", ModelOutput: valid.ModelOutput},
		{Action: app.ConversationRouteTool, ToolName: valid.ToolName, ToolArguments: valid.ToolArguments, Acknowledge: true, ModelOutput: valid.ModelOutput},
		{Action: app.ConversationRouteTool, ToolName: valid.ToolName, ToolArguments: valid.ToolArguments, Reply: "Done.", ModelOutput: valid.ModelOutput},
	}
	for index, decision := range invalidDecisions {
		if validStoredRouteDecision(decision, context) {
			t.Fatalf("invalid direct tool decision %d was accepted: %+v", index, decision)
		}
	}

	ineligibleContext := context
	ineligibleContext.ReferenceTaskID = "q1234567"
	if validStoredRouteDecision(valid, ineligibleContext) {
		t.Fatal("direct tool decision was accepted for a task-associated context")
	}
}

func TestValidStoredRouteDecisionRejectsInvalidActionAndFieldCombinations(t *testing.T) {
	context := app.ConversationRouteContext{Content: "帮我检查一下接口的输入校验，并补上测试。"}
	for _, test := range []struct {
		name     string
		decision app.ConversationRouteDecision
	}{
		{
			name: "unknown action",
			decision: app.ConversationRouteDecision{
				Action: app.ConversationRouteAction("execute"),
			},
		},
		{
			name: "reply with explicit-new flag",
			decision: app.ConversationRouteDecision{
				Action: app.ConversationRouteReply, ExplicitNew: true, Reply: "The answer is ready.",
			},
		},
		{
			name: "new with target message",
			decision: app.ConversationRouteDecision{
				Action: app.ConversationRouteNew, ExplicitNew: true, TargetMessageID: "m1234567",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if validStoredRouteDecision(test.decision, context) {
				t.Fatalf("invalid stored route decision was accepted: %+v", test.decision)
			}
		})
	}
}
