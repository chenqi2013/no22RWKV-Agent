package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentapi "github.com/no22/RWKV-Agent/api"
	"github.com/no22/RWKV-Agent/internal/appstorage"
)

func resolvedWorkspace(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	resolved, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func testAppStore(t *testing.T) *appstorage.Store {
	t.Helper()
	root := t.TempDir()
	dataDirectory := filepath.Join(root, "data")
	return appstorage.New(appstorage.Paths{
		ConfigFile:       filepath.Join(root, "config", "settings.json"),
		DataDirectory:    dataDirectory,
		StateFile:        filepath.Join(root, "state", "app-state.json"),
		CacheDirectory:   filepath.Join(root, "cache"),
		ConversationData: filepath.Join(dataDirectory, "conversations"),
	})
}

func testConversation(t *testing.T, store *appstorage.Store, workspace, title, content string) appstorage.Conversation {
	t.Helper()
	conversation, err := appstorage.NewConversation(workspace, title)
	if err != nil {
		t.Fatal(err)
	}
	conversation.Messages = []appstorage.DisplayMessage{{ID: "message-1", Role: "user", Content: content}}
	conversation.Transcript = []agentapi.ConversationMessage{{Role: "user", Content: content}}
	if err := store.SaveConversation(conversation); err != nil {
		t.Fatal(err)
	}
	return conversation
}

func TestAppServiceBootstrapsSavedSettingsAndConversation(t *testing.T) {
	t.Parallel()
	workspace := resolvedWorkspace(t)
	store := testAppStore(t)
	config := agentapi.Config{
		Provider: agentapi.ProviderRWKVLightningCUDA,
		Endpoint: "https://example.test",
		Model:    "rwkv-test",
		Password: "plain-secret",
		Headers:  map[string]string{"X-Service-Key": "plain-header"},
	}
	if _, err := store.SaveProvider("", "", config, true); err != nil {
		t.Fatal(err)
	}
	conversation := testConversation(t, store, workspace, "Saved conversation", "hello")
	if _, err := store.RememberWorkspace(workspace); err != nil {
		t.Fatal(err)
	}
	if err := store.SetActiveConversation(workspace, conversation.ID); err != nil {
		t.Fatal(err)
	}

	service, err := agentapi.NewService(agentapi.Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	backend := newAppService(service, store)
	t.Cleanup(func() { _ = backend.Close() })

	bootstrap, err := backend.Bootstrap()
	if err != nil {
		t.Fatal(err)
	}
	if !bootstrap.HasConfig || bootstrap.Config.Password != "plain-secret" || bootstrap.Config.Headers["X-Service-Key"] != "plain-header" {
		t.Fatalf("saved config was not restored: %+v", bootstrap.Config)
	}
	if bootstrap.Status.State != agentapi.ModelReady || bootstrap.Status.Provider != config.Provider || bootstrap.Status.Model != config.Model {
		t.Fatalf("saved remote provider was not activated: %+v", bootstrap.Status)
	}
	if len(bootstrap.Providers) != 1 || bootstrap.ActiveProviderID == "" || bootstrap.Providers[0].ID != bootstrap.ActiveProviderID {
		t.Fatalf("saved provider profile was not restored: %+v", bootstrap.Providers)
	}
	if bootstrap.RuntimeProviderID != bootstrap.ActiveProviderID {
		t.Fatalf("restored remote provider was not marked as running: %+v", bootstrap)
	}
	if bootstrap.Conversation == nil || bootstrap.Conversation.ID != conversation.ID || len(bootstrap.Conversation.Messages) != 1 {
		t.Fatalf("active conversation was not restored: %+v", bootstrap.Conversation)
	}
}

func TestAppServiceSavesDraftWithoutReplacingRuntimeProvider(t *testing.T) {
	t.Parallel()
	workspace := resolvedWorkspace(t)
	store := testAppStore(t)
	activeConfig := agentapi.Config{
		Provider: agentapi.ProviderRWKVLightningCUDA, Endpoint: "https://active.test", Model: "active-model",
	}
	active, err := store.SaveProvider("", "", activeConfig, true)
	if err != nil {
		t.Fatal(err)
	}
	service, err := agentapi.NewService(agentapi.Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	backend := newAppService(service, store)
	t.Cleanup(func() { _ = backend.Close() })

	draftConfig := agentapi.Config{
		Provider: agentapi.ProviderRWKVLightningCUDA, Endpoint: "https://draft.test/v1/models", Model: "draft-model",
	}
	draft, err := backend.SaveProvider("", "Draft", draftConfig)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := backend.Bootstrap()
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.RuntimeProviderID != active.ID || bootstrap.ActiveProviderID != active.ID || bootstrap.Status.Model != activeConfig.Model {
		t.Fatalf("saving draft changed runtime provider: %+v", bootstrap)
	}

	status, err := backend.ConfigureProvider(t.Context(), draft.ID, "Draft", draftConfig)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != agentapi.ModelReady || status.Model != draftConfig.Model {
		t.Fatalf("configured status = %+v", status)
	}
	bootstrap, err = backend.Bootstrap()
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.RuntimeProviderID != draft.ID || bootstrap.ActiveProviderID != draft.ID {
		t.Fatalf("configured draft was not active: %+v", bootstrap)
	}

	edited := draftConfig
	edited.Model = "edited-model"
	if _, err := backend.SaveProvider(draft.ID, "Edited", edited); err != nil {
		t.Fatal(err)
	}
	bootstrap, err = backend.Bootstrap()
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.RuntimeProviderID != "" || bootstrap.Status.Model != draftConfig.Model {
		t.Fatalf("editing the running profile should leave an unmatched runtime snapshot: %+v", bootstrap)
	}
}

func TestAppServiceSwitchesWorkspaceAndRestoresItsConversation(t *testing.T) {
	t.Parallel()
	workspaceA := resolvedWorkspace(t)
	workspaceB := resolvedWorkspace(t)
	store := testAppStore(t)
	conversationA := testConversation(t, store, workspaceA, "Conversation A", "from A")
	conversationB := testConversation(t, store, workspaceB, "Conversation B", "from B")
	if _, err := store.RememberWorkspace(workspaceA); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RememberWorkspace(workspaceB); err != nil {
		t.Fatal(err)
	}
	if err := store.SetActiveConversation(workspaceA, conversationA.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetActiveConversation(workspaceB, conversationB.ID); err != nil {
		t.Fatal(err)
	}

	service, err := agentapi.NewService(agentapi.Options{Workspace: workspaceA})
	if err != nil {
		t.Fatal(err)
	}
	backend := newAppService(service, store)
	t.Cleanup(func() { _ = backend.Close() })

	bootstrap, err := backend.OpenWorkspace(workspaceB)
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.Status.Workspace != workspaceB || bootstrap.Status.State != agentapi.ModelIdle {
		t.Fatalf("workspace status = %+v", bootstrap.Status)
	}
	if bootstrap.Conversation == nil || bootstrap.Conversation.ID != conversationB.ID || bootstrap.Conversation.Messages[0].Content != "from B" {
		t.Fatalf("workspace conversation = %+v", bootstrap.Conversation)
	}
	foundActive := false
	for _, workspace := range bootstrap.Workspaces {
		if workspace.Path == workspaceB && workspace.Active {
			foundActive = true
		}
	}
	if !foundActive {
		t.Fatalf("recent workspaces = %+v", bootstrap.Workspaces)
	}
}

func TestAppServiceSwitchesWorkspaceWithoutDroppingRuntimeProvider(t *testing.T) {
	t.Parallel()
	workspaceA := resolvedWorkspace(t)
	workspaceB := resolvedWorkspace(t)
	store := testAppStore(t)
	config := agentapi.Config{
		Provider: agentapi.ProviderRWKVLightningCUDA,
		Endpoint: "https://example.test",
		Model:    "rwkv-test",
	}
	provider, err := store.SaveProvider("", "Remote", config, true)
	if err != nil {
		t.Fatal(err)
	}
	service, err := agentapi.NewService(agentapi.Options{Workspace: workspaceA})
	if err != nil {
		t.Fatal(err)
	}
	backend := newAppService(service, store)
	t.Cleanup(func() { _ = backend.Close() })

	bootstrap, err := backend.OpenWorkspace(workspaceB)
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.Status.Workspace != workspaceB || bootstrap.Status.State != agentapi.ModelReady || bootstrap.Status.Model != config.Model {
		t.Fatalf("workspace status = %+v", bootstrap.Status)
	}
	if bootstrap.RuntimeProviderID != provider.ID {
		t.Fatalf("runtime provider = %q, want %q", bootstrap.RuntimeProviderID, provider.ID)
	}
}

func TestParseOptionsTracksExplicitWorkspace(t *testing.T) {
	t.Parallel()
	implicit, err := parseOptions(nil)
	if err != nil {
		t.Fatal(err)
	}
	if implicit.workspaceExplicit {
		t.Fatal("default workspace was marked explicit")
	}
	explicitPath := resolvedWorkspace(t)
	explicit, err := parseOptions([]string{"--workspace", explicitPath})
	if err != nil {
		t.Fatal(err)
	}
	if !explicit.workspaceExplicit || explicit.workspace != explicitPath {
		t.Fatalf("explicit options = %+v", explicit)
	}
}

func TestStoredToolTraceKeepsCompactStatus(t *testing.T) {
	t.Parallel()
	trace := storedToolTrace(agentapi.Result{Steps: []agentapi.Step{
		{Number: 1, Stage: "decision"},
		{
			Number: 2, Tool: "spawn_agents", ToolArguments: `{"tasks":["docs","code"]}`,
			ToolResult: `{"large":"parent payload must not be copied"}`, ToolExecuted: true,
			Subagents: []agentapi.SubagentTrace{{
				Index: 1, Task: "docs", Status: "completed", Route: "inspect",
				Bundles: []string{"web"}, DurationMS: 850, Output: "found docs",
				Sources: []string{"https://example.test"},
				Steps: []agentapi.SubagentStep{{
					Number: 1, Tool: "web_fetch", Arguments: `{"urls":["https://example.test"]}`,
					Status: "completed", Retries: []agentapi.ToolRetryTrace{{
						Attempt: 1, MaxAttempts: 5, StatusCode: 503, DelayMS: 1000,
					}},
				}},
			}},
		},
		{
			Number: 3, Tool: "web_fetch", ToolError: "request timed out", ToolExecuted: true,
			ToolRetries: []agentapi.ToolRetryTrace{{Attempt: 1, MaxAttempts: 5, DelayMS: 500}},
		},
	}})
	if len(trace) != 2 {
		t.Fatalf("trace = %+v", trace)
	}
	if trace[0].Step != 2 || trace[0].Tool != "spawn_agents" || trace[0].Status != "completed" || trace[0].Error != "" ||
		trace[0].Arguments != `{"tasks":["docs","code"]}` || len(trace[0].Subagents) != 1 ||
		trace[0].Subagents[0].Steps[0].Tool != "web_fetch" || trace[0].Subagents[0].Output != "found docs" ||
		trace[0].Subagents[0].Steps[0].Retries[0].StatusCode != 503 ||
		trace[0].Subagents[0].Steps[0].Retries[0].DelayMS != 1000 {
		t.Fatalf("completed trace = %+v", trace[0])
	}
	encoded, err := json.Marshal(trace)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "parent payload must not be copied") {
		t.Fatalf("trace retained tool result: %s", encoded)
	}
	if trace[1].Status != "failed" || trace[1].Error != "request timed out" ||
		trace[1].Retries[0].StatusCode != 0 || trace[1].Retries[0].DelayMS != 500 {
		t.Fatalf("failed trace = %+v", trace[1])
	}
}

func TestPersistTurnKeepsFailedRunTrace(t *testing.T) {
	t.Parallel()
	workspace := resolvedWorkspace(t)
	store := testAppStore(t)
	service, err := agentapi.NewService(agentapi.Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	backend := newAppService(service, store)
	t.Cleanup(func() { _ = backend.Close() })

	result := agentapi.Result{
		Error:      "model service returned 503",
		DurationMS: 612,
		Steps: []agentapi.Step{{
			Number: 1, Stage: "model", ModelError: "model service returned 503",
			Request: &agentapi.PromptTrace{Prompt: "inspect failure", Bytes: 15},
		}},
	}
	if err := backend.persistTurn(nil, "inspect failure", "error", result.Error, result, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	backend.mu.Lock()
	active := backend.active
	backend.mu.Unlock()
	if active == nil || len(active.Messages) != 2 {
		t.Fatalf("persisted conversation = %+v", active)
	}
	failed := active.Messages[1]
	if failed.Role != "error" || failed.Trace == nil || failed.Trace.Error != result.Error ||
		len(failed.Trace.Steps) != 1 || failed.Trace.Steps[0].ModelError != result.Error {
		t.Fatalf("persisted failed message = %+v", failed)
	}

	reloaded, err := store.LoadConversation(active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Messages) != 2 || reloaded.Messages[1].Trace == nil || reloaded.Messages[1].Trace.Error != result.Error {
		t.Fatalf("reloaded failed message = %+v", reloaded.Messages)
	}
}

func TestPersistResponseCreatesSelectableSiblingBranches(t *testing.T) {
	t.Parallel()
	workspace := resolvedWorkspace(t)
	store := testAppStore(t)
	service, err := agentapi.NewService(agentapi.Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	backend := newAppService(service, store)
	t.Cleanup(func() { _ = backend.Close() })

	if err := backend.persistTurn(nil, "解释架构", "assistant", "第一个回答", agentapi.Result{}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	firstUserID := backend.active.Messages[0].ID
	firstResponseID := backend.active.Messages[1].ID
	backend.mu.Unlock()
	if err := backend.persistResponse(nil, "解释架构", "assistant", "第二个回答", agentapi.Result{}, time.Now().UTC(), firstUserID); err != nil {
		t.Fatal(err)
	}

	backend.mu.Lock()
	active := *backend.active
	backend.mu.Unlock()
	if len(active.Messages) != 3 || active.Messages[2].ParentID != firstUserID || active.ActiveLeafID != active.Messages[2].ID {
		t.Fatalf("branched conversation = %+v", active)
	}
	view, err := backend.SelectBranch(firstResponseID)
	if err != nil {
		t.Fatal(err)
	}
	if view.ActiveLeafID != firstResponseID || len(view.Messages) != 3 {
		t.Fatalf("selected branch view = %+v", view)
	}
}

func TestEditMessageCreatesSiblingUserBranch(t *testing.T) {
	t.Parallel()
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requestCount++
		content := "旧回答"
		if requestCount > 1 {
			content = "新回答"
		}
		encoded, _ := json.Marshal(content)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":` + string(encoded) + `}}]}` + "\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	workspace := resolvedWorkspace(t)
	store := testAppStore(t)
	service, err := agentapi.NewService(agentapi.Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	backend := newAppService(service, store)
	t.Cleanup(func() { _ = backend.Close() })
	stream := false
	if _, err := backend.ConfigureProvider(t.Context(), "", "Test", agentapi.Config{
		Provider: agentapi.ProviderRWKVLightningCUDA,
		Endpoint: server.URL + "/v1/chat/completions",
		Model:    "test-model",
		Stream:   &stream,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Chat(t.Context(), "解释旧架构"); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	userID := backend.active.Messages[0].ID
	backend.mu.Unlock()
	if _, err := backend.EditMessage(t.Context(), userID, "解释新架构"); err != nil {
		t.Fatal(err)
	}

	bootstrap, err := backend.Bootstrap()
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.Conversation == nil || len(bootstrap.Conversation.Messages) != 4 {
		t.Fatalf("edited conversation = %+v", bootstrap.Conversation)
	}
	messages := bootstrap.Conversation.Messages
	if messages[2].Role != "user" || messages[2].Content != "解释新架构" || messages[2].ParentID != messages[0].ParentID {
		t.Fatalf("edited user branch = %+v", messages[2])
	}
	if messages[3].ParentID != messages[2].ID || messages[3].Content != "新回答" || bootstrap.Conversation.ActiveLeafID != messages[3].ID {
		t.Fatalf("edited answer branch = %+v", messages[3])
	}
}

func TestBranchTranscriptRebuildsSelectedPathFromSegments(t *testing.T) {
	t.Parallel()
	conversation := appstorage.Conversation{
		Messages: []appstorage.DisplayMessage{
			{ID: "u1", Role: "user", Content: "first"},
			{ID: "a1", ParentID: "u1", Role: "assistant", Content: "one"},
			{ID: "u2", ParentID: "a1", Role: "user", Content: "second"},
			{ID: "a2", ParentID: "u2", Role: "assistant", Content: "two"},
			{ID: "a3", ParentID: "u2", Role: "assistant", Content: "alternate"},
		},
		BranchSegments: map[string][]agentapi.ConversationMessage{
			"a1": {{Role: "user", Content: "first"}, {Role: "assistant", Content: "one"}},
			"a2": {{Role: "user", Content: "second"}, {Role: "assistant", Content: "two"}},
			"a3": {{Role: "user", Content: "second"}, {Role: "assistant", Content: "alternate"}},
		},
	}
	history, err := branchTranscript(conversation, "a3")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 4 || history[1].Content != "one" || history[3].Content != "alternate" {
		t.Fatalf("branch history = %+v", history)
	}
}
