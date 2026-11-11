package main

import (
	"context"
	"fmt"

	"morde/server/internal/app"
	"morde/server/internal/model"
)

func main() {
	client, err := model.NewResponsesClient([]string{"api.openai.com"}, "http://127.0.0.1:18080")
	if err != nil {
		panic(err)
	}
	response, err := client.Complete(context.Background(), app.ModelRequest{
		Endpoint: app.ModelEndpoint{BaseURL: "https://api.openai.com/v1", APIKey: "local-diagnostic", ModelID: "gpt-4o"},
		Input: []app.ModelInput{{Type: "message", Role: "user", Text: `{"message":"G3_FETCH_PATH:reports/summary.txt"}`}},
		MaxOutputTokens: 256,
	})
	if err != nil {
		fmt.Printf("failed: %T: %v\n", err, err)
		return
	}
	fmt.Printf("ok: stop=%s usage_known=%t text=%s\n", response.StopReason, response.Usage.Known, response.Text)
}
