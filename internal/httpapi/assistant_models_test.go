package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"

	"github.com/gin-gonic/gin"
)

func TestAssistantModelPickerOmitsCodeBuddyCLIDefault(t *testing.T) {
	t.Setenv("AGENTFLOW_ASSISTANT_MODEL", "")
	t.Setenv("AGENTFLOW_ASSISTANT_MODELS", "default-model,model-a,model-b")
	h, user := workbenchFixture(t)
	stubCLIModelDiscovery(t)

	c, rec := workbenchContext(user, http.MethodGet, "/api/assistant/models", bytes.NewBuffer(nil))
	h.models(c)
	var catalog assistantModelCatalog
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &catalog) != nil {
		t.Fatalf("model picker: %d %s", rec.Code, rec.Body)
	}
	if catalog.Default != assistantDefaultModel || len(catalog.DefaultEfforts) == 0 ||
		assistantCatalogLists(catalog, assistantDefaultModel) || assistantCatalogLists(catalog, "default-model") || !assistantCatalogLists(catalog, "model-b") {
		t.Fatalf("model picker shows the wrong choices: %+v", catalog)
	}
	if got := assistantAllowedModel("", h.chatCatalog(context.Background(), user)); got != assistantDefaultModel {
		t.Fatalf("an omitted request model should still use the CLI fallback, got %q", got)
	}
	t.Setenv("AGENTFLOW_ASSISTANT_MODELS", "")
	c, rec = workbenchContext(user, http.MethodGet, "/api/assistant/models", bytes.NewBuffer(nil))
	h.models(c)
	var unconfigured assistantModelCatalog
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &unconfigured) != nil ||
		unconfigured.Default != assistantDefaultModel || unconfigured.Models == nil || len(unconfigured.Models) != 0 {
		t.Fatalf("unconfigured picker: %d %s", rec.Code, rec.Body)
	}
}

func TestAssistantModelsUseSharedKimiDatabaseAllowList(t *testing.T) {
	h, user := workbenchFixture(t)
	user.AgentCLICredentials = map[string]domain.AgentCLICredential{
		runtime.AgentCLIKimi: {Models: []domain.CLIModel{{ID: "kimi-for-coding-highspeed"}}},
	}
	if err := h.repo.PutUser(user); err != nil {
		t.Fatal(err)
	}
	user, _ = h.repo.UserByLoginName(user.LoginName)
	if err := h.repo.SetPlatformKimiKey(repo.PlatformKimiKeyUpdate{
		Provider: "kimi", APIKey: "shared-secret", BaseURL: "https://api.moonshot.cn/v1",
		Models: []string{"kimi-k3.1-pro", "kimi-k2.6"},
	}, 0); err != nil {
		t.Fatal(err)
	}
	stubCLIModelDiscovery(t)
	c, rec := workbenchContext(user, http.MethodGet, "/api/assistant/models", bytes.NewBuffer(nil))
	h.models(c)
	var catalog assistantModelCatalog
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &catalog) != nil {
		t.Fatalf("model picker: %d %s", rec.Code, rec.Body)
	}
	var kimi []assistantModel
	for _, model := range catalog.Models {
		if model.Engine == runtime.AgentCLIKimi {
			kimi = append(kimi, model)
		}
	}
	if len(kimi) != 2 || !assistantCatalogLists(catalog, "kimi:k3.1-pro") || !assistantCatalogLists(catalog, "kimi:kimi-k2.6") ||
		strings.Contains(rec.Body.String(), "highspeed") || strings.Contains(rec.Body.String(), "k3-256k") {
		t.Fatalf("shared Kimi picker models = %+v; response=%s", kimi, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "shared-secret") {
		t.Fatal("model catalog leaked the shared key")
	}
	c, rec = workbenchContext(user, http.MethodGet, "/api/assistant/models?executor=kimi", bytes.NewBuffer(nil))
	h.models(c)
	var providerCatalog assistantModelCatalog
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &providerCatalog) != nil ||
		len(providerCatalog.Models) != 2 || providerCatalog.Models[0].ID != "kimi-k3.1-pro" ||
		providerCatalog.Models[1].ID != "kimi-k2.6" || strings.Contains(rec.Body.String(), "highspeed") {
		t.Fatalf("shared Kimi provider catalog: %d %s", rec.Code, rec.Body.String())
	}
}

func TestCodeBuddyModelDiscoveryPerEnvironment(t *testing.T) {
	help := "  --model <model>  Model for the current session. Currently supported: (gpt-5.5, glm-5.3, echo, custom-local:glm-5.2, kimi-k3)\n"
	if got := parseCodeBuddyModels(help); !reflect.DeepEqual(got, []string{"gpt-5.5", "glm-5.3", "kimi-k3"}) {
		t.Fatalf("parsed %v", got)
	}
	t.Setenv("AGENTFLOW_ASSISTANT_MODEL", "")
	t.Setenv("AGENTFLOW_ASSISTANT_MODELS", "")
	calls := map[string]int{}
	old := discoverCLIModels
	discoverCLIModels = func(_ context.Context, environment, apiKey string) []string {
		calls[environment+"|"+apiKey]++
		if environment == "ioa" && apiKey != "" {
			return []string{"claude-sonnet-5", "claude-opus-5.5"}
		}
		if environment == "ioa" {
			return []string{"claude-sonnet-5"}
		}
		return []string{"gpt-5.5"}
	}
	t.Cleanup(func() {
		discoverCLIModels = old
		cliModelMu.Lock()
		cliModelCache = map[string]cliModelEntry{}
		cliModelMu.Unlock()
	})
	for range 2 {
		refreshCLIModels(context.Background(), "ioa", "")
	}
	refreshCLIModels(context.Background(), "", "")
	if calls["ioa|"] != 1 || calls["|"] != 1 {
		t.Fatalf("discovery must be cached per environment: %v", calls)
	}
	ioa := loadAssistantModels(context.Background(), "ioa")
	if ioa.Default != "default" || assistantAllowedModel("claude-sonnet-5", ioa) != "claude-sonnet-5" || assistantAllowedModel("gpt-5.5", ioa) != "default" || assistantAllowedModel("claude-opus-5.5", ioa) != "default" {
		t.Fatalf("ioa catalog: %+v", ioa)
	}
	for range 2 {
		refreshCLIModels(context.Background(), "ioa", "user-key")
	}
	if calls["ioa|user-key"] != 1 {
		t.Fatalf("keyed discovery must be cached per account: %v", calls)
	}
	if ioa = loadAssistantModels(context.Background(), "ioa"); assistantAllowedModel("claude-opus-5.5", ioa) != "claude-opus-5.5" {
		t.Fatalf("account models must be listed: %+v", ioa)
	}
	if intl := loadAssistantModels(context.Background(), ""); assistantAllowedModel("gpt-5.5", intl) != "gpt-5.5" {
		t.Fatalf("international catalog: %+v", intl)
	}
}
