package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	agentapi "github.com/no22/RWKV-Agent/api"
	"github.com/no22/RWKV-Agent/internal/appstorage"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// The presentation message and tool-trace shapes are owned by appstorage (the
// same values are persisted and served to the UI verbatim); these aliases keep
// the short names the UI layer reads. DisplayMessage is one presentation-level
// message retained for the desktop UI; the complete Harness transcript is
// stored separately and never trimmed down to this view.
type (
	DisplayMessage = appstorage.DisplayMessage
	ToolTrace      = appstorage.ToolTrace
	ToolRetryTrace = appstorage.ToolRetryTrace
	SubagentStep   = appstorage.SubagentStep
	SubagentTrace  = appstorage.SubagentTrace
)

type ConversationSummary struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	UpdatedAt time.Time `json:"updatedAt"`
	Pinned    bool      `json:"pinned"`
}

type ConversationView struct {
	ID           string           `json:"id"`
	Title        string           `json:"title"`
	Messages     []DisplayMessage `json:"messages"`
	ActiveLeafID string           `json:"activeLeafId,omitempty"`
	CreatedAt    time.Time        `json:"createdAt"`
	UpdatedAt    time.Time        `json:"updatedAt"`
}

type WorkspaceItem struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Active    bool   `json:"active"`
}

type StoragePaths struct {
	ConfigFile     string `json:"configFile"`
	DataDirectory  string `json:"dataDirectory"`
	StateFile      string `json:"stateFile"`
	CacheDirectory string `json:"cacheDirectory"`
}

type AppBootstrap struct {
	Status            agentapi.Status            `json:"status"`
	Config            agentapi.Config            `json:"config"`
	HasConfig         bool                       `json:"hasConfig"`
	Providers         []appstorage.SavedProvider `json:"providers"`
	ActiveProviderID  string                     `json:"activeProviderId,omitempty"`
	RuntimeProviderID string                     `json:"runtimeProviderId,omitempty"`
	Conversations     []ConversationSummary      `json:"conversations"`
	Conversation      *ConversationView          `json:"conversation,omitempty"`
	Workspaces        []WorkspaceItem            `json:"workspaces"`
	Paths             StoragePaths               `json:"paths"`
	Warning           string                     `json:"warning,omitempty"`
}

// AppService binds durable application state to the public Agent API.
type AppService struct {
	operation         sync.Mutex
	mu                sync.Mutex
	service           *agentapi.Service
	storage           *appstorage.Store
	session           *agentapi.Session
	active            *appstorage.Conversation
	config            agentapi.Config
	hasConfig         bool
	runtimeProviderID string
	app               *application.App
	warning           string
	closed            bool
}

func newAppService(service *agentapi.Service, storage *appstorage.Store) *AppService {
	backend := &AppService{service: service, storage: storage}
	if err := storage.Prepare(); err != nil {
		backend.warning = fmt.Sprintf("准备应用存储目录失败：%v", err)
	}
	if settings, err := storage.LoadSettings(); err != nil {
		backend.warning = joinWarning(backend.warning, fmt.Sprintf("读取设置失败：%v", err))
	} else if cfg := settings.Provider; strings.TrimSpace(cfg.Model) != "" {
		cfg = desktopStreamingConfig(cfg)
		backend.config = cfg
		backend.hasConfig = true
		// 远端配置很轻量（不发网络请求，连通性检查在"测试连接"里），启动时自动连上
		// 上次使用的远端档案，省去手动重连；本地模型加载较重，仍需用户显式加载。
		if cfg.Provider != agentapi.ProviderLocal {
			if _, err := service.Configure(context.Background(), cfg, func(agentapi.Status) {}); err != nil {
				backend.warning = joinWarning(backend.warning, fmt.Sprintf("自动连接上次的远端失败：%v", err))
			} else {
				backend.runtimeProviderID = settings.ActiveID
			}
		}
	}
	// 不要在这里持久化工作区：启动时的工作区可能是默认值（用户主目录），
	// 只有用户通过“打开工作区”主动选择后才应该被记住。
	backend.loadActiveConversation()
	return backend
}

func (s *AppService) setApplication(app *application.App) {
	s.mu.Lock()
	s.app = app
	s.mu.Unlock()
}

// Bootstrap returns all durable state needed to hydrate the frontend.
func (s *AppService) Bootstrap() (AppBootstrap, error) {
	s.operation.Lock()
	defer s.operation.Unlock()
	return s.bootstrap()
}

// Status returns the current non-secret model state.
func (s *AppService) Status() agentapi.Status {
	s.mu.Lock()
	service := s.service
	s.mu.Unlock()
	return service.Status()
}

// PreviewSystemPrompt renders the system prompts a session for this draft
// configuration would start from. It never touches the running connection.
func (s *AppService) PreviewSystemPrompt(config agentapi.Config) (agentapi.AgentPromptPreview, error) {
	s.mu.Lock()
	service := s.service
	s.mu.Unlock()
	return service.PreviewAgentPrompt(config)
}

// ConfigureProvider 保存指定档案并把它切换为真实运行连接。传入 id 后允许修改地址或模型而不复制档案。
func (s *AppService) ConfigureProvider(ctx context.Context, id string, label string, config agentapi.Config) (agentapi.Status, error) {
	s.operation.Lock()
	defer s.operation.Unlock()
	return s.configureProvider(ctx, id, label, config)
}

func (s *AppService) configureProvider(ctx context.Context, id string, label string, config agentapi.Config) (agentapi.Status, error) {
	config = desktopStreamingConfig(config)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return agentapi.Status{}, fmt.Errorf("application service is closed")
	}
	service := s.service
	old := s.session
	s.session = nil
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	status, err := service.Configure(ctx, config, func(value agentapi.Status) {
		s.emit("model:status", value)
	})
	s.emit("model:status", status)
	if err != nil {
		return status, err
	}
	saved, err := s.storage.SaveProvider(id, label, config, true)
	if err != nil {
		return status, fmt.Errorf("模型已配置，但保存设置失败: %w", err)
	}
	s.mu.Lock()
	s.config = config
	s.hasConfig = true
	s.runtimeProviderID = saved.ID
	s.mu.Unlock()
	return status, nil
}

// SaveProvider 只保存编辑草稿，不连接服务，也不替换当前会话使用的 provider。
func (s *AppService) SaveProvider(id string, label string, config agentapi.Config) (appstorage.SavedProvider, error) {
	config = desktopStreamingConfig(config)
	if err := validateProviderDraft(config); err != nil {
		return appstorage.SavedProvider{}, err
	}
	s.operation.Lock()
	defer s.operation.Unlock()
	saved, err := s.storage.SaveProvider(id, label, config, false)
	if err != nil {
		return appstorage.SavedProvider{}, err
	}
	s.mu.Lock()
	status := s.service.Status()
	if status.State == agentapi.ModelReady && reflect.DeepEqual(s.config, saved.Config) {
		s.runtimeProviderID = saved.ID
	} else if s.runtimeProviderID == saved.ID {
		s.runtimeProviderID = ""
	}
	s.mu.Unlock()
	return saved, nil
}

func desktopStreamingConfig(config agentapi.Config) agentapi.Config {
	switch config.Provider {
	case agentapi.ProviderRWKVLightningPython, agentapi.ProviderRWKVLightningCUDA:
		enabled := true
		config.Stream = &enabled
	}
	return config
}

// ListRemoteModels verifies remote connectivity and returns model identifiers.
func (s *AppService) ListRemoteModels(ctx context.Context, config agentapi.Config) ([]agentapi.RemoteModel, error) {
	s.mu.Lock()
	service := s.service
	s.mu.Unlock()
	return service.ListRemoteModels(ctx, config)
}

// ActivateProvider 切换到一条已保存的连接档案：取出其 Config 后复用 Configure 完成连接并置为 active。
func (s *AppService) ActivateProvider(ctx context.Context, id string) (agentapi.Status, error) {
	settings, err := s.storage.LoadSettings()
	if err != nil {
		return agentapi.Status{}, err
	}
	for _, entry := range settings.Providers {
		if entry.ID == id {
			return s.ConfigureProvider(ctx, entry.ID, entry.Label, entry.Config)
		}
	}
	return agentapi.Status{}, fmt.Errorf("找不到该连接档案")
}

// DeleteProvider 删除一条连接档案并返回刷新后的引导状态（不影响当前正在运行的会话）。
func (s *AppService) DeleteProvider(id string) (AppBootstrap, error) {
	s.operation.Lock()
	defer s.operation.Unlock()
	if _, err := s.storage.RemoveProvider(id); err != nil {
		return AppBootstrap{}, err
	}
	s.mu.Lock()
	if s.runtimeProviderID == id {
		s.runtimeProviderID = ""
	}
	s.mu.Unlock()
	return s.bootstrap()
}

// Chat runs and durably commits one Agent turn in the active conversation.
func (s *AppService) Chat(ctx context.Context, prompt string) (agentapi.Result, error) {
	if strings.TrimSpace(prompt) == "" {
		return agentapi.Result{}, fmt.Errorf("message is required")
	}
	s.operation.Lock()
	defer s.operation.Unlock()
	session, err := s.ensureSession(ctx)
	if err != nil {
		return agentapi.Result{}, err
	}
	turnStarted := time.Now().UTC()
	result, err := session.RunWithObserver(ctx, prompt, func(event agentapi.Event) {
		s.emit("agent:event", event)
	})
	role := "assistant"
	content := result.Output
	if err != nil {
		result.Error = err.Error()
		role = "error"
		content = err.Error()
		s.emit("agent:error", err.Error())
	}
	if persistErr := s.persistTurn(session, prompt, role, content, result, turnStarted); persistErr != nil {
		if err != nil {
			return result, errors.Join(err, persistErr)
		}
		return result, persistErr
	}
	return result, err
}

// Regenerate creates a sibling response from the context immediately before
// the selected response's user message.
func (s *AppService) Regenerate(ctx context.Context, responseID string) (agentapi.Result, error) {
	s.operation.Lock()
	defer s.operation.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return agentapi.Result{}, fmt.Errorf("application service is closed")
	}
	active := s.active
	service := s.service
	s.mu.Unlock()
	if active == nil {
		return agentapi.Result{}, fmt.Errorf("conversation is not active")
	}
	response := conversationMessage(active.Messages, responseID)
	if response == nil || (response.Role != "assistant" && response.Role != "error") {
		return agentapi.Result{}, fmt.Errorf("response branch was not found")
	}
	user := conversationMessage(active.Messages, response.ParentID)
	if user == nil || user.Role != "user" {
		return agentapi.Result{}, fmt.Errorf("response branch has no user message")
	}
	prefix, err := branchHistoryBeforeUser(*active, *user)
	if err != nil {
		return agentapi.Result{}, err
	}
	created, err := service.NewSession(ctx)
	if err != nil {
		return agentapi.Result{}, err
	}
	if len(prefix) > 0 {
		if err := created.RestoreHistory(prefix); err != nil {
			_ = created.Close()
			return agentapi.Result{}, fmt.Errorf("恢复分支上下文失败: %w", err)
		}
	}
	turnStarted := time.Now().UTC()
	result, runErr := created.RunWithObserver(ctx, user.Content, func(event agentapi.Event) {
		s.emit("agent:event", event)
	})
	role := "assistant"
	content := result.Output
	if runErr != nil {
		result.Error = runErr.Error()
		role = "error"
		content = runErr.Error()
		s.emit("agent:error", runErr.Error())
	}
	if persistErr := s.persistResponse(created, user.Content, role, content, result, turnStarted, user.ID); persistErr != nil {
		_ = created.Close()
		if runErr != nil {
			return result, errors.Join(runErr, persistErr)
		}
		return result, persistErr
	}
	s.mu.Lock()
	old := s.session
	s.session = created
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return result, runErr
}

// EditMessage creates a sibling user message and generates its answer from
// the context immediately before the selected user message.
func (s *AppService) EditMessage(ctx context.Context, userID string, content string) (agentapi.Result, error) {
	prompt := strings.TrimSpace(content)
	if prompt == "" {
		return agentapi.Result{}, fmt.Errorf("message is required")
	}
	s.operation.Lock()
	defer s.operation.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return agentapi.Result{}, fmt.Errorf("application service is closed")
	}
	active := s.active
	service := s.service
	s.mu.Unlock()
	if active == nil {
		return agentapi.Result{}, fmt.Errorf("conversation is not active")
	}
	user := conversationMessage(active.Messages, userID)
	if user == nil || user.Role != "user" {
		return agentapi.Result{}, fmt.Errorf("user message was not found")
	}
	userParentID := user.ParentID
	prefix, err := branchHistoryBeforeUser(*active, *user)
	if err != nil {
		return agentapi.Result{}, err
	}
	created, err := service.NewSession(ctx)
	if err != nil {
		return agentapi.Result{}, err
	}
	if len(prefix) > 0 {
		if err := created.RestoreHistory(prefix); err != nil {
			_ = created.Close()
			return agentapi.Result{}, fmt.Errorf("恢复分支上下文失败: %w", err)
		}
	}
	turnStarted := time.Now().UTC()
	result, runErr := created.RunWithObserver(ctx, prompt, func(event agentapi.Event) {
		s.emit("agent:event", event)
	})
	role := "assistant"
	responseContent := result.Output
	if runErr != nil {
		result.Error = runErr.Error()
		role = "error"
		responseContent = runErr.Error()
		s.emit("agent:error", runErr.Error())
	}
	editedUser := appstorage.DisplayMessage{
		ID: messageID(active.ID, len(active.Messages)), ParentID: userParentID, Role: "user",
		Content: prompt, CreatedAt: turnStarted,
	}
	active.Messages = append(active.Messages, editedUser)
	if persistErr := s.persistResponse(created, prompt, role, responseContent, result, turnStarted, editedUser.ID); persistErr != nil {
		_ = created.Close()
		if runErr != nil {
			return result, errors.Join(runErr, persistErr)
		}
		return result, persistErr
	}
	s.mu.Lock()
	old := s.session
	s.session = created
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return result, runErr
}

// SelectBranch makes one stored response the active continuation point.
func (s *AppService) SelectBranch(responseID string) (ConversationView, error) {
	s.operation.Lock()
	defer s.operation.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ConversationView{}, fmt.Errorf("application service is closed")
	}
	active := s.active
	s.mu.Unlock()
	if active == nil {
		return ConversationView{}, fmt.Errorf("conversation is not active")
	}
	response := conversationMessage(active.Messages, responseID)
	if response == nil || (response.Role != "assistant" && response.Role != "error") {
		return ConversationView{}, fmt.Errorf("response branch was not found")
	}
	history, err := branchTranscript(*active, responseID)
	if err != nil {
		return ConversationView{}, err
	}
	active.ActiveLeafID = responseID
	active.Transcript = append([]agentapi.ConversationMessage(nil), history...)
	if err := s.storage.SaveConversation(*active); err != nil {
		return ConversationView{}, fmt.Errorf("保存消息分支失败: %w", err)
	}
	saved, err := s.storage.LoadConversation(active.ID)
	if err != nil {
		return ConversationView{}, err
	}
	s.mu.Lock()
	old := s.session
	s.session = nil
	s.active = &saved
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return conversationView(saved), nil
}

func (s *AppService) persistTurn(session *agentapi.Session, prompt, role, content string, result agentapi.Result, turnStarted time.Time) error {
	return s.persistResponse(session, prompt, role, content, result, turnStarted, "")
}

func (s *AppService) persistResponse(session *agentapi.Session, prompt, role, content string, result agentapi.Result, turnStarted time.Time, userID string) error {
	s.mu.Lock()
	active := s.active
	workspace := s.service.Status().Workspace
	s.mu.Unlock()
	if active == nil {
		created, createErr := appstorage.NewConversation(workspace, prompt)
		if createErr != nil {
			return createErr
		}
		active = &created
	}
	if active.BranchSegments == nil {
		active.BranchSegments = make(map[string][]agentapi.ConversationMessage)
	}
	if userID == "" {
		user := appstorage.DisplayMessage{
			ID: messageID(active.ID, len(active.Messages)), ParentID: active.ActiveLeafID, Role: "user",
			Content: strings.TrimSpace(prompt), CreatedAt: turnStarted,
		}
		userID = user.ID
		active.Messages = append(active.Messages, user)
	}
	response := appstorage.DisplayMessage{
		ID: messageID(active.ID, len(active.Messages)), ParentID: userID, Role: role, Content: content,
		Meta:       fmt.Sprintf("%d 步 · %.1f 秒", len(result.Steps), float64(result.DurationMS)/1000),
		Trajectory: storedToolTrace(result),
		Trace:      &result,
		CreatedAt:  time.Now().UTC(),
	}
	active.Messages = append(active.Messages, response)
	user := conversationMessage(active.Messages, userID)
	if user == nil {
		return fmt.Errorf("response branch has no user message")
	}
	prefix, err := branchHistoryBeforeUser(*active, *user)
	if err != nil {
		return err
	}
	var history []agentapi.ConversationMessage
	if session != nil {
		history = session.History()
	}
	active.ActiveLeafID = response.ID
	active.Transcript = append([]agentapi.ConversationMessage(nil), history...)
	segment := history
	if len(prefix) <= len(history) && reflect.DeepEqual(history[:len(prefix)], prefix) {
		segment = history[len(prefix):]
	}
	active.BranchSegments[response.ID] = append([]agentapi.ConversationMessage(nil), segment...)
	if err := s.storage.SaveConversation(*active); err != nil {
		return fmt.Errorf("保存对话失败: %w", err)
	}
	savedValue, err := s.storage.LoadConversation(active.ID)
	if err != nil {
		return fmt.Errorf("重新读取已保存对话失败: %w", err)
	}
	active = &savedValue
	if err := s.storage.SetActiveConversation(workspace, active.ID); err != nil {
		return fmt.Errorf("保存当前对话失败: %w", err)
	}
	s.mu.Lock()
	s.active = active
	s.mu.Unlock()
	s.emit("conversation:saved", conversationView(*active))
	return nil
}

// NewConversation starts a blank durable conversation while preserving the
// configured provider.
func (s *AppService) NewConversation() error {
	s.operation.Lock()
	defer s.operation.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("application service is closed")
	}
	old := s.session
	s.session = nil
	s.active = nil
	workspace := s.service.Status().Workspace
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	if err := s.storage.SetActiveConversation(workspace, ""); err != nil {
		return err
	}
	s.emit("conversation:reset")
	return nil
}

// OpenConversation selects an existing conversation for display and semantic
// continuation. Its Harness transcript is restored lazily when the next turn
// runs.
func (s *AppService) OpenConversation(id string) (ConversationView, error) {
	s.operation.Lock()
	defer s.operation.Unlock()
	value, err := s.storage.LoadConversation(id)
	if err != nil {
		return ConversationView{}, err
	}
	hydrateLegacyBranchHistory(&value)
	s.mu.Lock()
	workspace := s.service.Status().Workspace
	if !appstorage.SamePath(value.Workspace, workspace) {
		s.mu.Unlock()
		return ConversationView{}, fmt.Errorf("conversation belongs to another workspace")
	}
	old := s.session
	s.session = nil
	s.active = &value
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	if err := s.storage.SetActiveConversation(workspace, id); err != nil {
		return ConversationView{}, err
	}
	return conversationView(value), nil
}

// RenameConversation updates a conversation's display title.
func (s *AppService) RenameConversation(id string, title string) error {
	s.operation.Lock()
	defer s.operation.Unlock()
	if err := s.storage.RenameConversation(id, strings.TrimSpace(title)); err != nil {
		return err
	}
	return nil
}

// SetConversationPinned pins or unpins a conversation in the sidebar.
func (s *AppService) SetConversationPinned(id string, pinned bool) error {
	s.operation.Lock()
	defer s.operation.Unlock()
	return s.storage.SetConversationPinned(id, pinned)
}

// DeleteConversation removes one saved conversation.
func (s *AppService) DeleteConversation(id string) error {
	s.operation.Lock()
	defer s.operation.Unlock()
	s.mu.Lock()
	active := s.active != nil && s.active.ID == id
	workspace := s.service.Status().Workspace
	var old *agentapi.Session
	if active {
		old = s.session
		s.session = nil
		s.active = nil
	}
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	if err := s.storage.DeleteConversation(id); err != nil {
		return err
	}
	if active {
		return s.storage.SetActiveConversation(workspace, "")
	}
	return nil
}

// ChooseWorkspace opens the platform-native directory picker. Server builds
// return the Wails "file dialogs not available" error.
func (s *AppService) ChooseWorkspace() (AppBootstrap, error) {
	s.mu.Lock()
	app := s.app
	s.mu.Unlock()
	if app == nil {
		return AppBootstrap{}, fmt.Errorf("application is not ready")
	}
	selected, err := app.Dialog.OpenFile().
		CanChooseFiles(false).
		CanChooseDirectories(true).
		SetTitle("选择工作区").
		PromptForSingleSelection()
	if err != nil {
		return AppBootstrap{}, err
	}
	if strings.TrimSpace(selected) == "" {
		return s.Bootstrap()
	}
	return s.OpenWorkspace(selected)
}

// OpenWorkspace switches to an existing directory while retaining the current
// provider. Sessions are recreated so their tools use the new workspace.
func (s *AppService) OpenWorkspace(path string) (AppBootstrap, error) {
	s.operation.Lock()
	defer s.operation.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return AppBootstrap{}, fmt.Errorf("application service is closed")
	}
	s.mu.Unlock()
	s.mu.Lock()
	service := s.service
	oldSession := s.session
	oldWorkspace := service.Status().Workspace
	s.mu.Unlock()
	status, err := service.SetWorkspace(path)
	if err != nil {
		return AppBootstrap{}, err
	}
	if _, err := s.storage.RememberWorkspace(status.Workspace); err != nil {
		_, _ = service.SetWorkspace(oldWorkspace)
		s.mu.Lock()
		s.session = nil
		s.mu.Unlock()
		return AppBootstrap{}, err
	}
	s.mu.Lock()
	s.session = nil
	s.active = nil
	s.mu.Unlock()
	if oldSession != nil {
		_ = oldSession.Close()
	}
	s.loadActiveConversation()
	s.emit("model:status", status)
	return s.bootstrap()
}

// ExportTrajectory shows a native save dialog and writes the given JSONL
// content to the chosen path. Returns the written path, or "" when the user
// cancels the dialog.
func (s *AppService) ExportTrajectory(content string) (string, error) {
	app := s.app
	if app == nil || app.Dialog == nil {
		return "", errors.New("桌面窗口不可用")
	}
	dialog := app.Dialog.SaveFile()
	dialog.SetOptions(&application.SaveFileDialogOptions{
		Title:    "导出轨迹",
		Filename: "rwkv-agent-trajectory.jsonl",
		Message:  "将当前可见的轨迹保存为 JSONL",
	})
	dialog.AddFilter("JSONL", "*.jsonl")
	dialog.AddFilter("所有文件", "*")
	path, err := dialog.PromptForSingleSelection()
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", nil // 用户取消
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("写入轨迹文件失败: %w", err)
	}
	return path, nil
}

// Close releases the current conversation and model provider.
func (s *AppService) Close() error {
	s.operation.Lock()
	defer s.operation.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	session := s.session
	service := s.service
	s.session = nil
	s.mu.Unlock()
	if session != nil {
		_ = session.Close()
	}
	return service.Close()
}

func (s *AppService) ensureSession(ctx context.Context) (*agentapi.Session, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("application service is closed")
	}
	if s.session != nil {
		result := s.session
		s.mu.Unlock()
		return result, nil
	}
	service := s.service
	active := s.active
	s.mu.Unlock()
	created, err := service.NewSession(ctx)
	if err != nil {
		return nil, err
	}
	if active != nil && len(active.Transcript) > 0 {
		if err := created.RestoreHistory(active.Transcript); err != nil {
			_ = created.Close()
			return nil, fmt.Errorf("恢复对话失败: %w", err)
		}
	}
	s.mu.Lock()
	if s.closed || s.service != service {
		s.mu.Unlock()
		_ = created.Close()
		return nil, fmt.Errorf("workspace changed while creating conversation")
	}
	s.session = created
	s.mu.Unlock()
	return created, nil
}

func (s *AppService) bootstrap() (AppBootstrap, error) {
	s.mu.Lock()
	service := s.service
	config := s.config
	hasConfig := s.hasConfig
	runtimeProviderID := s.runtimeProviderID
	active := s.active
	warning := s.warning
	s.mu.Unlock()
	status := service.Status()
	summaries, err := s.storage.ListConversations(status.Workspace)
	if err != nil {
		return AppBootstrap{}, err
	}
	state, err := s.storage.LoadState()
	if err != nil {
		return AppBootstrap{}, err
	}
	settings, err := s.storage.LoadSettings()
	if err != nil {
		return AppBootstrap{}, err
	}
	paths := s.storage.Paths()
	result := AppBootstrap{
		Status: status, Config: config, HasConfig: hasConfig,
		Providers: settings.Providers, ActiveProviderID: settings.ActiveID, RuntimeProviderID: runtimeProviderID,
		Conversations: conversationSummaries(summaries),
		Workspaces:    workspaceItems(state.RecentWorkspaces, status.Workspace),
		Paths: StoragePaths{
			ConfigFile: paths.ConfigFile, DataDirectory: paths.DataDirectory,
			StateFile: paths.StateFile, CacheDirectory: paths.CacheDirectory,
		},
		Warning: warning,
	}
	if active != nil {
		view := conversationView(*active)
		result.Conversation = &view
	}
	return result, nil
}

func validateProviderDraft(config agentapi.Config) error {
	if strings.TrimSpace(config.Model) == "" {
		return fmt.Errorf("模型 ID 或本地模型路径不能为空")
	}
	switch config.Provider {
	case agentapi.ProviderLocal:
		return nil
	case agentapi.ProviderChatCompletions, agentapi.ProviderRWKVLightningPython, agentapi.ProviderRWKVLightningCUDA:
		parsed, err := url.Parse(strings.TrimSpace(config.Endpoint))
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("API 地址必须是不含凭据的 HTTP(S) 绝对地址")
		}
		return nil
	default:
		return fmt.Errorf("不支持的 Provider %q", config.Provider)
	}
}

func (s *AppService) loadActiveConversation() {
	s.mu.Lock()
	workspace := s.service.Status().Workspace
	s.mu.Unlock()
	id, err := s.storage.ActiveConversation(workspace)
	if err != nil {
		s.mu.Lock()
		s.warning = joinWarning(s.warning, fmt.Sprintf("读取应用状态失败：%v", err))
		s.mu.Unlock()
		return
	}
	if id == "" {
		return
	}
	value, err := s.storage.LoadConversation(id)
	if err != nil || !appstorage.SamePath(value.Workspace, workspace) {
		return
	}
	hydrateLegacyBranchHistory(&value)
	s.mu.Lock()
	s.active = &value
	s.mu.Unlock()
}

func (s *AppService) emit(name string, data ...any) {
	s.mu.Lock()
	app := s.app
	s.mu.Unlock()
	if app != nil {
		app.Event.Emit(name, data...)
	}
}

func conversationSummaries(values []appstorage.Summary) []ConversationSummary {
	result := make([]ConversationSummary, 0, len(values))
	for _, value := range values {
		result = append(result, ConversationSummary{ID: value.ID, Title: value.Title, UpdatedAt: value.UpdatedAt, Pinned: value.Pinned})
	}
	return result
}

func conversationView(value appstorage.Conversation) ConversationView {
	messages := make([]DisplayMessage, 0, len(value.Messages))
	for _, message := range value.Messages {
		messages = append(messages, DisplayMessage{
			ID: message.ID, ParentID: message.ParentID, Role: message.Role, Content: message.Content, Meta: message.Meta,
			Trajectory: message.Trajectory, Trace: message.Trace, CreatedAt: message.CreatedAt,
		})
	}
	return ConversationView{
		ID: value.ID, Title: value.Title, Messages: messages, ActiveLeafID: value.ActiveLeafID,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

func conversationMessage(messages []appstorage.DisplayMessage, id string) *appstorage.DisplayMessage {
	for index := range messages {
		if messages[index].ID == id {
			return &messages[index]
		}
	}
	return nil
}

func branchHistoryBeforeUser(value appstorage.Conversation, user appstorage.DisplayMessage) ([]agentapi.ConversationMessage, error) {
	if user.ParentID == "" {
		return nil, nil
	}
	return branchTranscript(value, user.ParentID)
}

func branchTranscript(value appstorage.Conversation, responseID string) ([]agentapi.ConversationMessage, error) {
	var segments [][]agentapi.ConversationMessage
	visited := make(map[string]bool)
	for responseID != "" {
		if visited[responseID] {
			return nil, fmt.Errorf("response branch contains a cycle")
		}
		visited[responseID] = true
		response := conversationMessage(value.Messages, responseID)
		if response == nil || (response.Role != "assistant" && response.Role != "error") {
			return nil, fmt.Errorf("response branch was not found")
		}
		segment, ok := value.BranchSegments[responseID]
		if !ok {
			return nil, fmt.Errorf("response branch has no restorable context")
		}
		segments = append(segments, segment)
		user := conversationMessage(value.Messages, response.ParentID)
		if user == nil || user.Role != "user" {
			return nil, fmt.Errorf("response branch has no user message")
		}
		responseID = user.ParentID
	}
	var history []agentapi.ConversationMessage
	for index := len(segments) - 1; index >= 0; index-- {
		history = append(history, segments[index]...)
	}
	return history, nil
}

func hydrateLegacyBranchHistory(value *appstorage.Conversation) {
	if value == nil || len(value.Messages) < 2 || len(value.Transcript) == 0 {
		return
	}
	for index := 1; index < len(value.Messages); index++ {
		if value.Messages[index].ParentID != value.Messages[index-1].ID {
			return
		}
	}
	if value.BranchSegments == nil {
		value.BranchSegments = make(map[string][]agentapi.ConversationMessage)
	}
	cursor := 0
	for index, message := range value.Messages {
		if message.Role != "assistant" && message.Role != "error" {
			continue
		}
		user := conversationMessage(value.Messages, message.ParentID)
		if user == nil || user.Role != "user" {
			continue
		}
		userStart := transcriptUserIndex(value.Transcript, cursor, user.Content)
		if userStart < 0 {
			continue
		}
		end := len(value.Transcript)
		for next := index + 1; next < len(value.Messages); next++ {
			if value.Messages[next].Role != "user" {
				continue
			}
			if found := transcriptUserIndex(value.Transcript, userStart+1, value.Messages[next].Content); found >= 0 {
				end = found
			}
			break
		}
		value.BranchSegments[message.ID] = append([]agentapi.ConversationMessage(nil), value.Transcript[userStart:end]...)
		cursor = end
	}
}

func transcriptUserIndex(history []agentapi.ConversationMessage, start int, content string) int {
	for index := max(start, 0); index < len(history); index++ {
		if history[index].Role == "user" && history[index].Content == content {
			return index
		}
	}
	return -1
}

func storedToolTrace(result agentapi.Result) []appstorage.ToolTrace {
	trace := make([]appstorage.ToolTrace, 0, len(result.Steps))
	for _, step := range result.Steps {
		if strings.TrimSpace(step.Tool) == "" {
			continue
		}
		status := "completed"
		if step.ToolError != "" {
			status = "failed"
		}
		trace = append(trace, appstorage.ToolTrace{
			Step: step.Number, Tool: step.Tool, Arguments: step.ToolArguments,
			Status: status, Error: step.ToolError, Retries: storedToolRetries(step.ToolRetries),
			Subagents: storedSubagentTrace(step.Subagents),
		})
	}
	return trace
}

func storedSubagentTrace(values []agentapi.SubagentTrace) []appstorage.SubagentTrace {
	if len(values) == 0 {
		return nil
	}
	result := make([]appstorage.SubagentTrace, 0, len(values))
	for _, value := range values {
		child := appstorage.SubagentTrace{
			Index: value.Index, Task: value.Task, Status: value.Status, Error: value.Error,
			Route: value.Route, Bundles: append([]string(nil), value.Bundles...),
			DurationMS: value.DurationMS, Output: value.Output,
			Sources: append([]string(nil), value.Sources...),
		}
		for _, step := range value.Steps {
			child.Steps = append(child.Steps, appstorage.SubagentStep{
				Step: step.Number, Tool: step.Tool, Arguments: step.Arguments,
				Status: step.Status, Error: step.Error, Retries: storedToolRetries(step.Retries),
			})
		}
		result = append(result, child)
	}
	return result
}

func storedToolRetries(values []agentapi.ToolRetryTrace) []appstorage.ToolRetryTrace {
	if len(values) == 0 {
		return nil
	}
	result := make([]appstorage.ToolRetryTrace, 0, len(values))
	for _, value := range values {
		result = append(result, appstorage.ToolRetryTrace{
			Attempt: value.Attempt, MaxAttempts: value.MaxAttempts,
			StatusCode: value.StatusCode, DelayMS: value.DelayMS,
		})
	}
	return result
}

func workspaceItems(paths []string, active string) []WorkspaceItem {
	result := make([]WorkspaceItem, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		result = append(result, WorkspaceItem{
			Path: path, Name: filepath.Base(path), Available: err == nil && info.IsDir(), Active: appstorage.SamePath(path, active),
		})
	}
	return result
}

func messageID(conversationID string, index int) string {
	return fmt.Sprintf("%s-%d", conversationID, index+1)
}

func joinWarning(left, right string) string {
	if left == "" {
		return right
	}
	return left + "；" + right
}
