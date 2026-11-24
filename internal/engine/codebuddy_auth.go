package engine

import (
	"fmt"
	"slices"
	"strings"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
)

const (
	codeBuddyAPIKeyEnv   = "CODEBUDDY_API_KEY"
	codeBuddyInternetEnv = "CODEBUDDY_INTERNET_ENVIRONMENT"
)

// resolveCodeBuddyCredential returns the responsible user's own key, or the
// platform's shared key when the user has none (see repo.SharedCodeBuddyKey).
func resolveCodeBuddyCredential(s *repo.Store, userID int64) runtime.CodeBuddyCredential {
	if s == nil || userID <= 0 {
		return runtime.CodeBuddyCredential{}
	}
	user, ok := s.User(userID)
	if !ok {
		return runtime.CodeBuddyCredential{}
	}
	if strings.TrimSpace(user.CodeBuddyAPIKey) != "" {
		return runtime.CodeBuddyCredential{APIKey: user.CodeBuddyAPIKey, InternetEnvironment: user.CodeBuddyInternetEnvironment}
	}
	shared, ok := s.SharedCodeBuddyKey()
	if !ok || (shared.SponsorID != 0 && shared.SponsorID == user.ID) {
		return runtime.CodeBuddyCredential{InternetEnvironment: user.CodeBuddyInternetEnvironment}
	}
	return runtime.CodeBuddyCredential{APIKey: shared.APIKey, InternetEnvironment: shared.InternetEnvironment, Sponsored: true, SponsoredModels: shared.Models}
}

const sponsoredRunEvent = "model.sponsored"

// sponsoredExecutor limits a CodeBuddy node running on the shared key to the
// models the administrator allowed: the role's model when it is one of them,
// the default otherwise, and tells the user why the role's model is not used.
func (e *Engine) sponsoredExecutor(runID string, ex domain.ExecutorDefinition, models []string, sink func(domain.TelemetryEvent)) domain.ExecutorDefinition {
	configured := strings.TrimSpace(ex.Model)
	key := repo.SharedCodeBuddyKey{Models: models}
	model, allowed := key.Model(configured)
	if !allowed {
		model, _ = key.Model("")
	}
	if model == "" {
		model = repo.SponsoredCodeBuddyModel
	}
	ex.Model = model
	message := "责任人未配置个人 CodeBuddy API Key，本节点使用平台共享 Key，模型 " + model
	if configured != "" && configured != model {
		message += "（角色配置的 " + configured + " 不在平台允许的模型中，本次不生效）"
	}
	sink(domain.TelemetryEvent{Category: "model", Name: sponsoredRunEvent, Phase: "start", Message: message, Provider: ex.Kind, Model: ex.Model, Attributes: map[string]any{"configuredModel": configured}})
	if current, ok := e.repo.RunState(runID); ok && !hasRunEvent(current, sponsoredRunEvent) {
		e.updateRun(runID, func(r *domain.Run) {
			if !hasRunEvent(*r, sponsoredRunEvent) {
				e.event(r, "", sponsoredRunEvent, "责任人未配置个人 CodeBuddy API Key，本次运行使用平台共享 Key，只能使用平台管理员允许的模型（"+strings.Join(models, "、")+"）。在个人配置中配置自己的 Key 后，可使用角色配置的任意模型")
			}
		})
	}
	return ex
}

// sharedAgentCLIExecutor applies the platform allow-list to shared DSH/Kimi
// credentials. Unlike CodeBuddy's historical fallback, an explicitly
// configured but disallowed model fails clearly instead of silently changing.
func (e *Engine) sharedAgentCLIExecutor(runID string, ex domain.ExecutorDefinition, models []string, sink func(domain.TelemetryEvent)) (domain.ExecutorDefinition, error) {
	models = append([]string(nil), models...)
	if len(models) == 0 {
		return ex, fmt.Errorf("平台共享 %s Key 未配置可用模型，请管理员检查共享 Key 设置", runtime.AgentCLILabel(ex.Kind))
	}
	configured := strings.TrimSpace(ex.Model)
	if configured == "" || configured == "default" {
		ex.Model = models[0]
	} else {
		allowed := false
		for _, model := range models {
			if configured == model {
				allowed = true
				break
			}
		}
		if !allowed {
			return ex, fmt.Errorf("SOP 节点配置的 %s 模型 %q 不在平台共享 Key 允许列表中（%s）", runtime.AgentCLILabel(ex.Kind), configured, strings.Join(models, "、"))
		}
		ex.Model = configured
	}

	message := fmt.Sprintf("需求责任人未配置个人 %s API Key，本节点使用平台共享 Key，模型 %s", runtime.AgentCLILabel(ex.Kind), ex.Model)
	if sink != nil {
		sink(domain.TelemetryEvent{
			Category: "model", Name: "model.shared", Phase: "start", Message: message,
			Provider: ex.Kind, Model: ex.Model,
			Attributes: map[string]any{"credentialSource": "platform_shared", "allowedModels": models},
		})
	}
	eventType := "model.shared." + ex.Kind
	if current, ok := e.repo.RunState(runID); ok && !hasRunEvent(current, eventType) {
		e.updateRun(runID, func(r *domain.Run) {
			if !hasRunEvent(*r, eventType) {
				e.event(r, "", eventType, message)
			}
		})
	}
	return ex, nil
}

func hasRunEvent(run domain.Run, typ string) bool {
	return slices.ContainsFunc(run.Events, func(event domain.RunEvent) bool { return event.Type == typ })
}
