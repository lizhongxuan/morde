package worker

import (
	"strconv"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

func TestCloudDSHWebResearchToolDenialBoundsPersistedCalls(t *testing.T) {
	t.Run("allows one fallback attempt and blocks a third persisted attempt", func(t *testing.T) {
		history := newCloudDSHCheckpointHistory("session_1")
		history.calls["search_1"] = webResearchSearchCheckpoint(1, "first query")
		history.calls["search_2"] = webResearchSearchCheckpoint(2, "second query")
		if denial := cloudDSHWebResearchToolDenial(history, "search_2", cloudDSHWebResearchSearchToolName,
			[]byte(`{"query":"second query","category":"news","time_range":"week"}`), false); denial != "" {
			t.Fatalf("second distinct search was denied: %s", denial)
		}
		history.calls["search_3"] = webResearchSearchCheckpoint(3, "third query")
		if denial := cloudDSHWebResearchToolDenial(history, "search_3", cloudDSHWebResearchSearchToolName,
			[]byte(`{"query":"third query"}`), false); denial != cloudDSHWebResearchBudgetFeedback {
			t.Fatalf("third search denial = %q, want bounded feedback", denial)
		}
	})

	t.Run("blocks distinct queries after a successful search", func(t *testing.T) {
		history := newCloudDSHCheckpointHistory("session_1")
		history.calls["search_1"] = webResearchSearchCheckpoint(1, "first query")
		history.calls["search_1"].status = app.AgentToolSucceeded
		history.calls["search_1"].resultSeen = true
		history.calls["search_2"] = webResearchSearchCheckpoint(2, "second query")
		if denial := cloudDSHWebResearchToolDenial(history, "search_2", cloudDSHWebResearchSearchToolName,
			[]byte(`{"query":"second query"}`), false); denial != cloudDSHWebResearchBudgetFeedback {
			t.Fatalf("second search after success denial = %q, want bounded feedback", denial)
		}
	})

	t.Run("blocks duplicate normalized search", func(t *testing.T) {
		history := newCloudDSHCheckpointHistory("session_1")
		history.calls["search_1"] = webResearchSearchCheckpoint(1, "Today   Technology News")
		history.calls["search_2"] = webResearchSearchCheckpoint(2, " today technology news ")
		if denial := cloudDSHWebResearchToolDenial(history, "search_2", cloudDSHWebResearchSearchToolName,
			[]byte(`{"query":" today technology news "}`), false); denial != cloudDSHWebResearchBudgetFeedback {
			t.Fatalf("duplicate search denial = %q, want bounded feedback", denial)
		}
	})

	t.Run("does not spend the successful-search allowance twice and permits one failed-search retry", func(t *testing.T) {
		history := newCloudDSHCheckpointHistory("session_1")
		history.calls["search_1"] = webResearchSearchCheckpoint(1, "first query")
		history.calls["search_1"].status = app.AgentToolSucceeded
		history.calls["search_1"].resultSeen = true
		history.calls["search_guarded"] = webResearchSearchCheckpoint(2, "first query")
		history.calls["search_guarded"].resultSeen = true
		history.calls["search_guarded"].resultGuardDenied = true
		history.calls["search_3"] = webResearchSearchCheckpoint(3, "second query")
		if denial := cloudDSHWebResearchToolDenial(history, "search_3", cloudDSHWebResearchSearchToolName,
			[]byte(`{"query":"second query"}`), false); denial != cloudDSHWebResearchBudgetFeedback {
			t.Fatalf("new query after a successful search denial = %q, want bounded feedback", denial)
		}

		failed := newCloudDSHCheckpointHistory("session_1")
		failed.calls["search_failed"] = webResearchSearchCheckpoint(1, "same query")
		failed.calls["search_failed"].status = app.AgentToolFailed
		failed.calls["search_failed"].resultSeen = true
		failed.calls["search_retry"] = webResearchSearchCheckpoint(2, "same query")
		if denial := cloudDSHWebResearchToolDenial(failed, "search_retry", cloudDSHWebResearchSearchToolName,
			[]byte(`{"query":"same query"}`), false); denial != "" {
			t.Fatalf("known failed search should permit the one remaining attempt: %s", denial)
		}
	})

	t.Run("limits read windows per URL", func(t *testing.T) {
		history := newCloudDSHCheckpointHistory("session_1")
		history.calls["read_1"] = webResearchReadCheckpoint(1, `{"url":"https://news.example.test/a","max_chars":1200}`)
		history.calls["read_2"] = webResearchReadCheckpoint(2,
			`{"url":"https://news.example.test/a#article","start_char":1200,"max_chars":1200}`)
		if denial := cloudDSHWebResearchToolDenial(history, "read_2", cloudDSHWebResearchReadToolName,
			[]byte(`{"url":"https://news.example.test/a#article","start_char":1200,"max_chars":1200}`), false); denial != "" {
			t.Fatalf("second window for same page was denied: %s", denial)
		}
		history.calls["read_3"] = webResearchReadCheckpoint(3,
			`{"url":"https://news.example.test/a","start_char":2400,"max_chars":1200}`)
		if denial := cloudDSHWebResearchToolDenial(history, "read_3", cloudDSHWebResearchReadToolName,
			[]byte(`{"url":"https://news.example.test/a","start_char":2400,"max_chars":1200}`), false); denial != cloudDSHWebResearchBudgetFeedback {
			t.Fatalf("third window for the same page denial = %q, want bounded feedback", denial)
		}
	})

	t.Run("limits total reads and distinct source pages", func(t *testing.T) {
		history := newCloudDSHCheckpointHistory("session_1")
		for _, entry := range []struct {
			id  string
			seq int64
			url string
		}{
			{"read_1", 1, "https://news.example.test/a"},
			{"read_2", 2, "https://news.example.test/a"},
			{"read_3", 3, "https://news.example.test/b"},
			{"read_4", 4, "https://news.example.test/b"},
		} {
			start := 0
			if entry.seq == 2 {
				start = 1200
			}
			history.calls[entry.id] = webResearchReadCheckpoint(entry.seq,
				`{"url":"`+entry.url+`","start_char":`+strconv.Itoa(start)+`}`)
		}
		history.calls["read_5"] = webResearchReadCheckpoint(5, `{"url":"https://news.example.test/d"}`)
		if denial := cloudDSHWebResearchToolDenial(history, "read_5", cloudDSHWebResearchReadToolName,
			[]byte(`{"url":"https://news.example.test/d"}`), false); denial != cloudDSHWebResearchBudgetFeedback {
			t.Fatalf("fifth total read denial = %q, want bounded feedback", denial)
		}

		distinctURLs := newCloudDSHCheckpointHistory("session_1")
		distinctURLs.calls["read_1"] = webResearchReadCheckpoint(1, `{"url":"https://news.example.test/a"}`)
		distinctURLs.calls["read_2"] = webResearchReadCheckpoint(2, `{"url":"https://news.example.test/b"}`)
		distinctURLs.calls["read_3"] = webResearchReadCheckpoint(3, `{"url":"https://news.example.test/c"}`)
		if denial := cloudDSHWebResearchToolDenial(distinctURLs, "read_3", cloudDSHWebResearchReadToolName,
			[]byte(`{"url":"https://news.example.test/c"}`), false); denial != cloudDSHWebResearchBudgetFeedback {
			t.Fatalf("third distinct source page denial = %q, want bounded feedback", denial)
		}

		totalHistory := newCloudDSHCheckpointHistory("session_1")
		for _, entry := range []struct {
			id  string
			seq int64
			url string
		}{
			{"read_1", 1, "https://news.example.test/a"},
			{"read_2", 2, "https://news.example.test/a"},
			{"read_3", 3, "https://news.example.test/b"},
			{"read_4", 4, "https://news.example.test/b"},
			{"read_5", 5, "https://news.example.test/c"},
			{"read_6", 6, "https://news.example.test/c"},
		} {
			start := 0
			if entry.seq%2 == 0 {
				start = 1200
			}
			totalHistory.calls[entry.id] = webResearchReadCheckpoint(entry.seq,
				`{"url":"`+entry.url+`","start_char":`+strconv.Itoa(start)+`}`)
		}
		totalHistory.calls["read_7"] = webResearchReadCheckpoint(7, `{"url":"https://news.example.test/b","start_char":2400}`)
		if denial := cloudDSHWebResearchToolDenial(totalHistory, "read_7", cloudDSHWebResearchReadToolName,
			[]byte(`{"url":"https://news.example.test/b","start_char":2400}`), false); denial != cloudDSHWebResearchBudgetFeedback {
			t.Fatalf("fifth read denial = %q, want bounded feedback", denial)
		}
	})

	t.Run("fails closed on malformed checkpoint and permits only the same recovery identity", func(t *testing.T) {
		history := newCloudDSHCheckpointHistory("session_1")
		history.calls["bad_history"] = &cloudDSHToolCall{name: cloudDSHWebResearchSearchToolName,
			arguments: []byte(`not-json`), callSeq: 1, callSeen: true}
		history.calls["search_new"] = webResearchSearchCheckpoint(2, "new query")
		if denial := cloudDSHWebResearchToolDenial(history, "search_new", cloudDSHWebResearchSearchToolName,
			[]byte(`{"query":"new query"}`), false); denial != cloudDSHWebResearchBudgetFeedback {
			t.Fatalf("malformed checkpoint history denial = %q, want bounded feedback", denial)
		}
		if denial := cloudDSHWebResearchToolDenial(history, "bad_history", cloudDSHWebResearchSearchToolName,
			[]byte(`not-json`), true); denial != "" {
			t.Fatalf("same recovery identity must reach reconciliation, got denial %q", denial)
		}
		if denial := cloudDSHWebResearchToolDenial(history, "missing_call", cloudDSHWebResearchSearchToolName,
			[]byte(`{"query":"new query"}`), true); denial != cloudDSHWebResearchBudgetFeedback {
			t.Fatalf("unmatched recovery identity denial = %q, want bounded feedback", denial)
		}
	})
}

func TestCloudDSHWebResearchProfileIsPersistedAndRevisionScoped(t *testing.T) {
	value := task.Task{ConversationContext: &task.ConversationContext{AgentToolProfile: task.AgentToolProfileWebResearch}}
	if !cloudDSHWebResearchProfileActive(value) {
		t.Fatal("initial persisted web_research profile was not active")
	}
	revision := int64(1)
	value.CurrentRevision = &revision
	if cloudDSHWebResearchProfileActive(value) {
		t.Fatal("profile must not constrain a later user revision")
	}
	value.CurrentRevision = nil
	value.ConversationContext.AgentToolProfile = ""
	if cloudDSHWebResearchProfileActive(value) {
		t.Fatal("ordinary Task must not inherit the web research guard")
	}
}

func webResearchSearchCheckpoint(sequence int64, query string) *cloudDSHToolCall {
	return &cloudDSHToolCall{name: cloudDSHWebResearchSearchToolName,
		arguments: []byte(`{"query":"` + query + `"}`), callSeq: sequence, callSeen: true}
}

func webResearchReadCheckpoint(sequence int64, arguments string) *cloudDSHToolCall {
	return &cloudDSHToolCall{name: cloudDSHWebResearchReadToolName,
		arguments: []byte(arguments), callSeq: sequence, callSeen: true}
}
