package worker

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/text/unicode/norm"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

const (
	cloudDSHWebResearchMaxSearchCalls       = 2
	cloudDSHWebResearchMaxReadCalls         = 6
	cloudDSHWebResearchMaxReadURLs          = 3
	cloudDSHWebResearchMaxReadWindowsPerURL = 2
)

const cloudDSHWebResearchBudgetFeedback = "The bounded web research allowance is exhausted or this exact request was already attempted. Stop repeating search/read calls, synthesize the available evidence, and state any gaps in the user's language."

func cloudDSHWebResearchProfileActive(value task.Task) bool {
	return value.ConversationContext != nil && initialCloudDSHTaskRevision(value) &&
		value.ConversationContext.AgentToolProfile == task.AgentToolProfileWebResearch
}

func cloudDSHWebResearchPriorBlocksDuplicate(call *cloudDSHToolCall) bool {
	if call == nil || call.resultGuardDenied {
		return false
	}
	if !call.resultSeen || call.resultUnknown || call.status == "" {
		return true
	}
	return call.status == app.AgentToolSucceeded
}

// cloudDSHWebResearchToolDenial is an authoritative Worker-side dispatch
// fence. It uses the DSH checkpoint history because that history is persisted
// before the Runtime emits a tool_call frame, so the limit survives process
// restart and includes calls whose operation result is not yet known.
// A matching recovery call is exempt from quota checks: it reconciles the
// original call identity and cannot create a second dispatch.
func cloudDSHWebResearchToolDenial(history *cloudDSHCheckpointHistory, currentCallID, toolName string,
	arguments []byte, recovering bool) string {
	if toolName != cloudDSHWebResearchSearchToolName && toolName != cloudDSHWebResearchReadToolName {
		return ""
	}
	if history == nil || currentCallID == "" {
		return cloudDSHWebResearchBudgetFeedback
	}
	currentCall := history.calls[currentCallID]
	if currentCall == nil || !currentCall.callSeen || currentCall.name != toolName || currentCall.callSeq < 1 {
		return cloudDSHWebResearchBudgetFeedback
	}
	if recovering {
		if bytes.Equal(currentCall.arguments, arguments) {
			return ""
		}
		return cloudDSHWebResearchBudgetFeedback
	}

	currentSignature, currentURL, ok := cloudDSHWebResearchCallSignature(toolName, arguments)
	if !ok {
		return cloudDSHWebResearchBudgetFeedback
	}

	searchCalls, readCalls := 0, 0
	readURLs := make(map[string]struct{})
	readWindowsByURL := make(map[string]int)
	for callID, call := range history.calls {
		if callID == currentCallID || call == nil || !call.callSeen || call.callSeq >= currentCall.callSeq || call.resultGuardDenied {
			continue
		}
		switch call.name {
		case cloudDSHWebResearchSearchToolName:
			searchCalls++
			prior, _, valid := cloudDSHWebResearchCallSignature(call.name, call.arguments)
			if !valid || toolName == cloudDSHWebResearchSearchToolName && prior == currentSignature &&
				cloudDSHWebResearchPriorBlocksDuplicate(call) {
				return cloudDSHWebResearchBudgetFeedback
			}
		case cloudDSHWebResearchReadToolName:
			readCalls++
			prior, priorURL, valid := cloudDSHWebResearchCallSignature(call.name, call.arguments)
			if !valid {
				return cloudDSHWebResearchBudgetFeedback
			}
			readURLs[priorURL] = struct{}{}
			readWindowsByURL[priorURL]++
			if toolName == cloudDSHWebResearchReadToolName && prior == currentSignature &&
				cloudDSHWebResearchPriorBlocksDuplicate(call) {
				return cloudDSHWebResearchBudgetFeedback
			}
		}
	}

	if toolName == cloudDSHWebResearchSearchToolName && searchCalls >= cloudDSHWebResearchMaxSearchCalls {
		return cloudDSHWebResearchBudgetFeedback
	}
	if toolName == cloudDSHWebResearchReadToolName &&
		(readCalls >= cloudDSHWebResearchMaxReadCalls || readWindowsByURL[currentURL] >= cloudDSHWebResearchMaxReadWindowsPerURL ||
			len(readURLs) >= cloudDSHWebResearchMaxReadURLs && !hasString(readURLs, currentURL)) {
		return cloudDSHWebResearchBudgetFeedback
	}
	return ""
}

func cloudDSHWebResearchCallSignature(toolName string, arguments []byte) (signature, pageURL string, ok bool) {
	if !json.Valid(arguments) {
		return "", "", false
	}
	switch toolName {
	case cloudDSHWebResearchSearchToolName:
		var values struct {
			Query         string `json:"query"`
			Category      string `json:"category"`
			TimeRange     string `json:"time_range"`
			LookupPurpose string `json:"lookup_purpose"`
		}
		decoder := json.NewDecoder(bytes.NewReader(arguments))
		if decoder.Decode(&values) != nil || strings.TrimSpace(values.Query) == "" {
			return "", "", false
		}
		query := strings.Join(strings.Fields(strings.ToLower(norm.NFKC.String(values.Query))), " ")
		encoded, err := json.Marshal([]string{query, values.Category, values.TimeRange, values.LookupPurpose})
		if err != nil {
			return "", "", false
		}
		return string(encoded), "", true
	case cloudDSHWebResearchReadToolName:
		var values struct {
			URL       string `json:"url"`
			StartChar *int   `json:"start_char"`
		}
		decoder := json.NewDecoder(bytes.NewReader(arguments))
		if decoder.Decode(&values) != nil || strings.TrimSpace(values.URL) == "" {
			return "", "", false
		}
		parsed, err := url.Parse(strings.TrimSpace(values.URL))
		if err != nil || strings.ToLower(parsed.Scheme) != "https" || parsed.Hostname() == "" || parsed.User != nil {
			return "", "", false
		}
		parsed.Scheme = "https"
		parsed.Host = strings.ToLower(parsed.Host)
		parsed.Fragment = ""
		canonicalURL := parsed.String()
		start := 0
		if values.StartChar != nil {
			start = *values.StartChar
		}
		if start < 0 {
			return "", "", false
		}
		return canonicalURL + "\x00" + strconv.Itoa(start), canonicalURL, true
	default:
		return "", "", false
	}
}

func hasString(values map[string]struct{}, value string) bool {
	_, ok := values[value]
	return ok
}
