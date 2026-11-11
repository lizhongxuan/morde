package model

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"morde/server/internal/app"
)

func TestC30G3LocalModelRouteDiagnostic(t *testing.T) {
	client, err := NewResponsesClient([]string{"api.openai.com"}, "http://127.0.0.1:18080")
	if err != nil {
		t.Fatal(err)
	}
	request := app.ModelRequest{
		Endpoint:        app.ModelEndpoint{BaseURL: "https://api.openai.com/v1", APIKey: "local-diagnostic", ModelID: "gpt-4o"},
		Input:           []app.ModelInput{{Type: "message", Role: "user", Text: `{"message":"G3_FETCH_PATH:reports/summary.txt"}`}},
		MaxOutputTokens: 256,
	}
	endpoint, err := client.endpointURL(request.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	body, err := encodeRequest(request, nil)
	if err != nil {
		t.Fatal(err)
	}
	providerRequest, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	providerRequest.Header.Set("Authorization", "Bearer "+request.Endpoint.APIKey)
	providerRequest.Header.Set("Content-Type", "application/json")
	providerRequest.Header.Set("Accept", "application/json")
	responseBody, err := client.httpClient.Do(providerRequest)
	if err != nil {
		t.Fatalf("local Responses API transport failed (%T): %v", err, err)
	}
	defer responseBody.Body.Close()
	data, err := readResponseBody(responseBody.Body, maxResponseBytes)
	if err != nil {
		t.Fatalf("read local Responses API body (%T): %v", err, err)
	}
	var result responseBodyJSON
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode local Responses API response: %v", err)
	}
	response, err := parseResponse(result)
	if err != nil || response.Text != `{"action":"new","task_id":"","question":"","reply":"","dependency_task_ids":[]}` ||
		response.StopReason != "stop" || !response.Usage.Known {
		t.Fatalf("local route response = %#v, err=%v", response, err)
	}
}
