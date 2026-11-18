package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/runtime"
)

// AI 对话 runs on CodeBuddy by default. A model ID prefixed with another CLI
// ("codex:", "claude-code:" or an agent CLI kind such as "opencode:") runs the
// turn on that CLI with the caller's own key and Base URL from 个人配置; the
// part after the prefix is passed to the CLI as-is.
const (
	assistantEngineCodeBuddy = "codebuddy"
	assistantEngineCodex     = "codex"
	assistantEngineClaude    = "claude-code"
)

// assistantCLIEngines are the CLIs other than CodeBuddy, in switch order.
func assistantCLIEngines() []string {
	return append([]string{assistantEngineCodex, assistantEngineClaude, "kimi"}, runtime.AgentCLIKinds...)
}

func assistantModelEngine(id string) (engine, model string) {
	id = strings.TrimSpace(id)
	for _, engine := range assistantCLIEngines() {
		if rest, ok := strings.CutPrefix(id, engine+":"); ok {
			return engine, strings.TrimSpace(rest)
		}
	}
	return assistantEngineCodeBuddy, id
}

// assistantEngineOption is one entry of the chat composer's CLI switch.
// Credential names the 个人配置 field that enables it.
type assistantEngineOption struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Configured bool   `json:"configured"`
	Credential string `json:"credential"`
}

// assistantEngineCredentialField is the 个人配置 field that enables a CLI.
func assistantEngineCredentialField(engine string) string {
	switch engine {
	case assistantEngineCodeBuddy:
		return "codeBuddyApiKey"
	case assistantEngineCodex:
		return "codexApiKey"
	case assistantEngineClaude:
		return "claudeCodeApiKey"
	}
	return "agentCli." + engine
}

// assistantEngineOptions lists every CLI, configured or not, so the switch
// also shows where to add a key. CodeBuddy counts as configured when the
// platform sponsors chats without one.
func assistantEngineOptions(user domain.User, sponsored bool) []assistantEngineOption {
	engines := append([]string{assistantEngineCodeBuddy}, assistantCLIEngines()...)
	out := make([]assistantEngineOption, 0, len(engines))
	for _, engine := range engines {
		key, _ := assistantEngineCredential(user, engine)

		out = append(out, assistantEngineOption{
			ID:         engine,
			Name:       assistantEngineLabel(engine),
			Configured: key != "" || (engine == assistantEngineCodeBuddy && sponsored),
			Credential: assistantEngineCredentialField(engine),
		})
	}
	return out
}

func assistantEngineLabel(engine string) string {
	switch engine {
	case assistantEngineCodex:
		return "Codex"
	case assistantEngineClaude:
		return "Claude Code"
	case assistantEngineCodeBuddy, "":
		return "CodeBuddy"
	case "deepseek", "dsh":
		return "DeepSeek"
	case "kimi":
		return "Kimi"
	}
	return runtime.AgentCLILabel(engine)
}

func assistantEngineCredential(user domain.User, engine string) (apiKey, baseURL string) {
	switch engine {
	case assistantEngineCodex:
		return strings.TrimSpace(user.CodexAPIKey), strings.TrimSpace(user.CodexBaseURL)
	case assistantEngineClaude:
		return strings.TrimSpace(user.ClaudeCodeAPIKey), strings.TrimSpace(user.ClaudeCodeBaseURL)
	case assistantEngineCodeBuddy:
		return strings.TrimSpace(user.CodeBuddyAPIKey), ""
	}
	cred := user.AgentCLICredentials[engine]
	if runtime.IsAgentCLI(engine) && engine != runtime.AgentCLICursor {
		baseURL = strings.TrimSpace(cred.BaseURL)
	}

	lowerEngine := strings.ToLower(engine)
	if strings.Contains(lowerEngine, "kimi") || strings.Contains(lowerEngine, "moonshot") {
		return strings.TrimSpace(cred.APIKey), strings.TrimSpace(cred.BaseURL)
	}
	if strings.Contains(lowerEngine, "deepseek") {
		return strings.TrimSpace(cred.APIKey), strings.TrimSpace(cred.BaseURL)
	}

	return strings.TrimSpace(cred.APIKey), baseURL
}

// addSharedAgentCLIKeys makes platform keys available to chat model discovery
// and execution. A user's personal key always takes precedence.
func (h *assistantHandlers) addSharedAgentCLIKeys(user domain.User) domain.User {
	if user.AgentCLICredentials == nil {
		user.AgentCLICredentials = map[string]domain.AgentCLICredential{}
	}
	for provider, cli := range map[string]string{"kimi": runtime.AgentCLIKimi, "deepseek": runtime.AgentCLIDeepSeek} {
		if key, _ := assistantEngineCredential(user, cli); key != "" {
			continue
		}
		shared, ok := h.repo.SharedKeyByProvider(provider)
		if !ok {
			continue
		}
		models := make([]domain.CLIModel, 0, len(shared.Models))
		for _, id := range shared.Models {
			models = append(models, domain.CLIModel{ID: id, Name: id})
		}
		user.AgentCLICredentials[cli] = domain.AgentCLICredential{
			APIKey: shared.APIKey, BaseURL: shared.InternetEnvironment,
			Protocol: runtime.AgentCLIProtocolChat, Models: models,
		}
	}
	return user
}

// assistantEngineProtocol is the API protocol an agent CLI speaks; "" for
// the other CLIs.
func assistantEngineProtocol(user domain.User, engine string) string {
	if !runtime.IsAgentCLI(engine) {
		return ""
	}
	return runtime.AgentCLIProtocol(engine, user.AgentCLICredentials[engine].Protocol)
}

// assistantEngineAnthropicModels reports whether the model list behind a
// Base URL is the Anthropic one ({base}/v1/models, x-api-key).
func assistantEngineAnthropicModels(engine, protocol string) bool {
	return engine == assistantEngineClaude || protocol == runtime.AgentCLIProtocolMessages
}

func assistantEngineCustomModels(user domain.User, engine string) []domain.CLIModel {
	switch engine {
	case assistantEngineCodex:
		return user.CodexModels
	case assistantEngineClaude:
		return user.ClaudeCodeModels
	}
	return user.AgentCLICredentials[engine].Models
}

// assistantEngineBuiltinModels is the executor catalog for a CLI, used on the
// official endpoint (no Base URL) or when the Base URL lists no model.
func assistantEngineBuiltinModels(engine string) []assistantDiscoveredModel {
	catalogKey := engine
	if engine == assistantEngineClaude {
		catalogKey = "claude"
	}
	var out []assistantDiscoveredModel
	for _, model := range executorModelCatalogs()[catalogKey].Models {
		out = append(out, assistantDiscoveredModel{ID: model.ID, Name: model.Name})
	}
	return out
}

// assistantValidEngineModel guards model IDs that reach a CLI argument or a
// TOML config override.
func assistantValidEngineModel(model string) bool {
	if model == "" || len(model) > 128 {
		return false
	}
	for _, r := range model {
		if r <= ' ' || r == 0x7f || r == '"' || r == '\'' || r == '`' || r == '\\' {
			return false
		}
	}
	return true
}

// assistantConversationEngine is the CLI that wrote the conversation's latest
// own reply, "" before the first one. Role replies (SpeakerAgentID) run on
// their own executors and do not count. A turn that failed before producing
// anything leaves no reply, so it does not pin the CLI.
func assistantConversationEngine(messages []assistantMessage, legacyExecutor ...string) string {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != "assistant" || m.SpeakerAgentID != "" || strings.TrimSpace(m.Model) == "" {
			continue
		}
		if assistantModelHasEnginePrefix(m.Model) {
			engine, _ := assistantModelEngine(m.Model)
			return engine
		}
		if legacy := assistantLegacyExecutor(legacyExecutor...); legacy != "" && legacy != assistantEngineCodeBuddy {
			return legacy
		}
		engine, _ := assistantModelEngine(m.Model)
		return engine
	}
	return ""
}

func assistantModelHasEnginePrefix(id string) bool {
	id = strings.TrimSpace(id)
	for _, engine := range append([]string{assistantEngineCodeBuddy}, assistantCLIEngines()...) {
		if _, ok := strings.CutPrefix(id, engine+":"); ok {
			return true
		}
	}
	return false
}

// assistantLegacyExecutor maps a session Executor saved before model prefixes.
// CodeBuddy and an empty value stay unset so an unprefixed model remains CodeBuddy.
func assistantLegacyExecutor(candidates ...string) string {
	for _, raw := range candidates {
		switch id := strings.ToLower(strings.TrimSpace(raw)); id {
		case assistantEngineCodex:
			return assistantEngineCodex
		case "claude", assistantEngineClaude:
			return assistantEngineClaude
		case "", assistantEngineCodeBuddy:
			continue
		default:
			for _, kind := range domain.AgentCLIExecutorKinds {
				if strings.EqualFold(kind, id) {
					return kind
				}
			}
		}
	}
	return ""
}

// assistantEngineSwitchRefusal keeps a conversation on one CLI.
func assistantEngineSwitchRefusal(locked, model string) string {
	engine, _ := assistantModelEngine(model)
	if locked == "" || locked == engine {
		return ""
	}
	return fmt.Sprintf("这个对话由 %s 回复，不能中途切换 CLI；请新建对话后使用 %s", assistantEngineLabel(locked), assistantEngineLabel(engine))
}

func assistantEngineMissingKey(engine string) string {
	return fmt.Sprintf("请先在个人配置中配置 %s API Key 后再使用 %s 模型", assistantEngineLabel(engine), assistantEngineLabel(engine))
}

func (h *assistantHandlers) assistantTurnUser(stored domain.User, model string) (domain.User, string, string) {
	engine, _ := assistantModelEngine(model)
	if engine == "kimi" || engine == runtime.AgentCLIDeepSeek {
		provider := engine
		cli := engine
		if engine == runtime.AgentCLIDeepSeek {
			provider = "deepseek"
		}
		apiKey, _ := assistantEngineCredential(stored, cli)
		if apiKey == "" {
			shared, found := h.repo.SharedKeyByProvider(provider)
			if !found {
				return stored, "", assistantEngineMissingKey(cli)
			}
			requested := model
			if _, value, ok := strings.Cut(model, ":"); ok {
				requested = value
			}
			if _, selected := assistantSponsoredModelFor(shared.Models, requested); !selected {
				return stored, "", fmt.Sprintf("平台共享 %s Key 未开放模型 %s", assistantEngineLabel(cli), strings.TrimPrefix(model, engine+":"))
			}
			if stored.AgentCLICredentials == nil {
				stored.AgentCLICredentials = map[string]domain.AgentCLICredential{}
			}
			stored.AgentCLICredentials[cli] = domain.AgentCLICredential{
				APIKey: shared.APIKey,
				BaseURL: shared.InternetEnvironment,
				Protocol: runtime.AgentCLIProtocolChat,
			}
		}
	}

	if engine != assistantEngineCodeBuddy {
		if key, _ := assistantEngineCredential(stored, engine); key == "" {
			return stored, "", assistantEngineMissingKey(engine)
		}
		user, _, _ := h.assistantChatUser(stored)
		return user, model, ""
	}
	
	user, shared, ok := h.assistantChatUser(stored)
	if !ok {
		return stored, "", "[engines.go]请先在个人配置中配置自己的 CodeBuddy API Key"
	}
	if len(shared) > 0 {
		forced, allowed := assistantSponsoredModelFor(shared, model)
		if !allowed {
			return stored, "", errAssistantSponsoredModel(shared)
		}
		model = forced
	}
	return user, model, ""
}

func assistantAllowedUserModel(id string, catalog assistantModelCatalog, user domain.User) string {
	lower := strings.ToLower(id)
	if strings.Contains(lower, "deepseek") || strings.Contains(lower, "kimi") || strings.Contains(lower, "codex") || strings.Contains(lower, "moonshot") {
		return id
	}

	engine, model := assistantModelEngine(id)
	if engine == assistantEngineCodeBuddy {
		return assistantAllowedModel(id, catalog)
	}
	if strings.Contains(engine, "deepseek") || strings.Contains(engine, "kimi") {
		return engine + ":" + model
	}
	if key, _ := assistantEngineCredential(user, engine); key == "" || !assistantValidEngineModel(model) {
		return catalog.Default
	}
	return engine + ":" + model
}

func assistantScheduledModel(id string, catalog assistantModelCatalog) string {
	engine, model := assistantModelEngine(id)
	if engine != assistantEngineCodeBuddy && assistantValidEngineModel(model) {
		return engine + ":" + model
	}
	return assistantAllowedModel(id, catalog)
}

func assistantEngineModels(ctx context.Context, user domain.User) []assistantModel {
	var out []assistantModel
	for _, engine := range assistantCLIEngines() {
		key, baseURL := assistantEngineCredential(user, engine)
		if key == "" {
			continue
		}
		protocol := assistantEngineProtocol(user, engine)
		if protocol == runtime.AgentCLIProtocolGemini {
			baseURL = ""
		}
		var listed []assistantDiscoveredModel
		for _, model := range assistantEngineCustomModels(user, engine) {
			if assistantValidEngineModel(model.ID) {
				listed = append(listed, assistantDiscoveredModel{ID: model.ID, Name: model.Name})
			}
		}
		if len(listed) == 0 && baseURL != "" {
			listed = cachedAssistantEngineDiscovery(ctx, assistantEngineAnthropicModels(engine, protocol), key, baseURL)
		}
		if engine == runtime.AgentCLICursor {
			listed = cachedCursorModels(key)
		}
		if len(listed) == 0 {
			listed = assistantEngineBuiltinModels(engine)
		}
		for _, model := range listed {
			name := strings.TrimSpace(model.Name)
			if name == "" {
				name = model.ID
			}
			id := engine + ":" + model.ID
			out = append(out, assistantModel{
				ID:      id,
				Name:    name,
				Family:  assistantModelFamily(model.ID),
				Engine:  engine,
				Efforts: assistantModelEfforts(id),
			})
		}
	}
	return out
}

type assistantDiscoveredModel struct {
	ID   string
	Name string
}

type assistantDiscoveryEntry struct {
	models  []assistantDiscoveredModel
	expires time.Time
}

const (
	assistantDiscoveryTTL        = 10 * time.Minute
	assistantDiscoveryFailureTTL = time.Minute
	assistantDiscoveryTimeout    = 4 * time.Second
	assistantDiscoveryMaxModels  = 100
)

var (
	assistantDiscoveryCache sync.Map
	assistantEngineDiscover = discoverAssistantEngineModels
)

func cachedAssistantEngineDiscovery(ctx context.Context, anthropic bool, apiKey, baseURL string) []assistantDiscoveredModel {
	sum := sha256.Sum256([]byte(fmt.Sprint(anthropic) + "\x00" + baseURL + "\x00" + apiKey))
	cacheKey := hex.EncodeToString(sum[:])
	now := time.Now()
	if cached, ok := assistantDiscoveryCache.Load(cacheKey); ok {
		if entry := cached.(assistantDiscoveryEntry); now.Before(entry.expires) {
			return entry.models
		}
	}
	ctx, cancel := context.WithTimeout(ctx, assistantDiscoveryTimeout)
	defer cancel()
	models, err := assistantEngineDiscover(ctx, anthropic, apiKey, baseURL)
	ttl := assistantDiscoveryTTL
	if err != nil || len(models) == 0 {
		models, ttl = nil, assistantDiscoveryFailureTTL
	}
	assistantDiscoveryCache.Store(cacheKey, assistantDiscoveryEntry{models: models, expires: now.Add(ttl)})
	return models
}

var (
	assistantCursorModelsCache    sync.Map
	assistantCursorModelsInflight sync.Map
	assistantCursorModels         = cursorAgentModels
)

func cachedCursorModels(apiKey string) []assistantDiscoveredModel {
	sum := sha256.Sum256([]byte("cursor\x00" + apiKey))
	cacheKey := hex.EncodeToString(sum[:])
	var stale []assistantDiscoveredModel
	if cached, ok := assistantCursorModelsCache.Load(cacheKey); ok {
		entry := cached.(assistantDiscoveryEntry)
		if time.Now().Before(entry.expires) {
			return entry.models
		}
		stale = entry.models
	}
	if _, running := assistantCursorModelsInflight.LoadOrStore(cacheKey, true); !running {
		go func() {
			defer assistantCursorModelsInflight.Delete(cacheKey)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			found, err := assistantCursorModels(ctx, apiKey)
			var models []assistantDiscoveredModel
			for _, model := range found {
				if assistantValidEngineModel(model.ID) && len(models) < assistantDiscoveryMaxModels {
					models = append(models, assistantDiscoveredModel{ID: model.ID, Name: model.Name})
				}
			}
			ttl := assistantDiscoveryTTL
			if err != nil || len(models) == 0 {
				models, ttl = stale, assistantDiscoveryFailureTTL
			}
			assistantCursorModelsCache.Store(cacheKey, assistantDiscoveryEntry{models: models, expires: time.Now().Add(ttl)})
		}()
	}
	return stale
}

func discoverAssistantEngineModels(ctx context.Context, anthropic bool, apiKey, baseURL string) ([]assistantDiscoveredModel, error) {
	endpoint := strings.TrimRight(baseURL, "/")
	if anthropic {
		endpoint += "/v1/models"
	} else {
		endpoint += "/models"
	}
	if !strings.HasPrefix(endpoint, "https://") && !strings.HasPrefix(endpoint, "http://") {
		return nil, fmt.Errorf("Base URL 必须以 http:// 或 https:// 开头")
	}
	schemes := []string{"bearer"}
	if anthropic {
		schemes = append(schemes, "x-api-key")
	}
	var lastErr error
	for _, scheme := range schemes {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		if scheme == "bearer" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		} else {
			req.Header.Set("x-api-key", apiKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			var urlErr *url.Error
			if errors.As(err, &urlErr) {
				err = urlErr.Err
			}
			return nil, fmt.Errorf("无法连接 %s：%w", endpoint, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			lastErr = fmt.Errorf("%s 拒绝了 API Key（HTTP %d）", endpoint, resp.StatusCode)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s 返回 HTTP %d", endpoint, resp.StatusCode)
		}
		var payload struct {
			Data []struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				DisplayName string `json:"display_name"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("%s 返回的不是模型列表", endpoint)
		}
		var out []assistantDiscoveredModel
		seen := map[string]bool{}
		for _, item := range payload.Data {
			id := strings.TrimSpace(item.ID)
			if !assistantValidEngineModel(id) || seen[id] {
				continue
			}
			seen[id] = true
			name := strings.TrimSpace(item.DisplayName)
			if name == "" {
				name = strings.TrimSpace(item.Name)
			}
			out = append(out, assistantDiscoveredModel{ID: id, Name: truncateRunes(name, 64)})
			if len(out) == assistantDiscoveryMaxModels {
				break
			}
		}
		return out, nil
	}
	return nil, lastErr
}

type assistantEngineState struct {
	Engine    string `json:"engine"`
	SessionID string `json:"sessionId"`
}

func readAssistantEngineState(root string) assistantEngineState {
	state := assistantEngineState{Engine: assistantEngineCodeBuddy}
	data, err := os.ReadFile(filepath.Join(root, "engine.json"))
	if err != nil || json.Unmarshal(data, &state) != nil || state.Engine == "" {
		return assistantEngineState{Engine: assistantEngineCodeBuddy}
	}
	return state
}

func writeAssistantEngineState(root string, state assistantEngineState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "engine.json"), data, 0o600)
}

func assistantRequestModel(executor, model string) string {
	model = strings.TrimSpace(model)
	if engine, _ := assistantModelEngine(model); engine != assistantEngineCodeBuddy {
		return model
	}
	switch assistantExecutor(executor) {
	case assistantEngineCodex:
		return assistantEngineCodex + ":" + model
	case "claude", assistantEngineClaude:
		return assistantEngineClaude + ":" + model
	default:
		if runtime.IsAgentCLI(executor) {
			return strings.TrimSpace(executor) + ":" + model
		}
	}
	return model
}
