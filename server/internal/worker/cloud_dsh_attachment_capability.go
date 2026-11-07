package worker

import (
	"encoding/json"
	"regexp"
	"strings"

	"morde/server/internal/app"
)

const cloudDSHAttachmentMarker = "[MORDE_TASK_ATTACHMENT_IDS_V1:"

// The harness adds a separate final marker block only for attachments that
// the fenced message store has delivered. Check the current request as well
// as restored history, so a model without the reader cannot invent file data.
func cloudDSHRequestHasUnavailableAttachments(raw json.RawMessage, definitions []app.AgentToolDefinition) bool {
	var request struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(raw, &request) != nil {
		return true
	}
	for _, message := range request.Messages {
		if message.Role != "user" || len(message.Content) < 2 {
			continue
		}
		last := message.Content[len(message.Content)-1]
		if last.Type != "text" || !strings.HasPrefix(last.Text, cloudDSHAttachmentMarker) {
			continue
		}
		if !strings.HasSuffix(last.Text, "]") {
			return true
		}
		ids := strings.Split(strings.TrimSuffix(strings.TrimPrefix(last.Text, cloudDSHAttachmentMarker), "]"), ",")
		if len(ids) == 0 || !validCloudDSHMessageAttachmentIDs(ids) {
			return true
		}
		for _, id := range ids {
			if !cloudDSHAttachmentReaderAccepts(definitions, id) {
				return true
			}
		}
	}
	return false
}

func cloudDSHAttachmentReaderAccepts(definitions []app.AgentToolDefinition, id string) bool {
	for _, definition := range definitions {
		if definition.Name != "morde_read_task_attachment" {
			continue
		}
		var schema struct {
			Properties struct {
				Attachment struct {
					Type    string   `json:"type"`
					Pattern string   `json:"pattern"`
					Enum    []string `json:"enum"`
				} `json:"attachment_id"`
			} `json:"properties"`
		}
		if json.Unmarshal(definition.Parameters, &schema) != nil || schema.Properties.Attachment.Type != "string" {
			return false
		}
		value := schema.Properties.Attachment
		if value.Enum != nil {
			matched := false
			for _, candidate := range value.Enum {
				matched = matched || candidate == id
			}
			if !matched {
				return false
			}
		}
		if value.Pattern != "" {
			pattern, err := regexp.Compile(value.Pattern)
			if err != nil || !pattern.MatchString(id) {
				return false
			}
		}
		return true
	}
	return false
}
