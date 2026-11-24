package repo

import (
	"agentflow/internal/domain"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"
	"gorm.io/gorm/clause"
)

// Users who have not configured their own CodeBuddy API Key use the shared
// key a platform administrator configured under 平台管理, limited to the
// models the administrator allowed. It covers chat conversations and SOP
// runs, so a new user can start working before configuring anything;
// scheduled prompts, reports, bots and run evaluation keep requiring the
// user's own key.
//
// Older deployments lent the key of the account named by
// AGENTFLOW_ASSISTANT_SPONSOR_LOGIN instead, limited to
// SponsoredCodeBuddyModel. That still applies while no shared key is
// configured.
const (
	CodeBuddySponsorLoginEnv = "AGENTFLOW_ASSISTANT_SPONSOR_LOGIN"
	SponsoredCodeBuddyModel  = "deepseek-v4.1-flash"
	// MaxSharedCodeBuddyModels bounds the allow list of the shared key.
	MaxSharedCodeBuddyModels = 32
)

// SharedCodeBuddyKey is the key users without their own borrow.
type SharedCodeBuddyKey struct {
	APIKey              string
	InternetEnvironment string
	// Models are the models it may run; the first is the default.
	Models []string
	// SponsorID is the lending account under AGENTFLOW_ASSISTANT_SPONSOR_LOGIN,
	// 0 for the key configured under 平台管理.
	SponsorID int64
}

// Model returns the model a shared-key turn runs: the requested model when it
// is allowed, the default when none is requested, and false otherwise.
func (k SharedCodeBuddyKey) Model(requested string) (string, bool) {
	if len(k.Models) == 0 {
		return "", false
	}
	requested = strings.TrimSpace(requested)
	if _, id, ok := strings.Cut(requested, ":"); ok {
		requested = id
	}
	if requested == "" {
		return k.Models[0], true
	}
	for _, id := range k.Models {
		if id == requested || strings.TrimPrefix(id, "kimi-") == requested || strings.TrimPrefix(id, "deepseek-") == requested {
			return id, true
		}
	}
	return "", false
}

// The key is stored like personal CodeBuddy keys in user_configs: the
// database is the trust boundary, and the key is never returned by the API.
type platformCodeBuddyKeyRow struct {
	ID                  int64      `gorm:"column:id;primaryKey;autoIncrement:false"`
	APIKey              string     `gorm:"column:api_key;type:text;not null"`
	InternetEnvironment string     `gorm:"column:internet_environment;size:64;not null;default:''"`
	Models              string     `gorm:"column:models;type:text;not null"`
	UpdatedBy           int64      `gorm:"column:updated_by;not null;default:0"`
	Updated             *time.Time `gorm:"column:updated_at"`
}

func (platformCodeBuddyKeyRow) TableName() string { return "platform_codebuddy_keys" }

// The table holds at most this one row.
const platformCodeBuddyKeyID = 1

// PlatformCodeBuddyKeyStatus is what administrators see; it never carries the key.
type PlatformCodeBuddyKeyStatus struct {
	Configured          bool       `json:"configured"`
	InternetEnvironment string     `json:"internetEnvironment"`
	Models              []string   `json:"models"`
	UpdatedAt           *time.Time `json:"updatedAt,omitempty"`
	UpdatedBy           string     `json:"updatedBy,omitempty"`
	LegacySponsor       string     `json:"legacySponsor,omitempty"`
	LegacySponsorModel  string     `json:"legacySponsorModel,omitempty"`
}

// PlatformCodeBuddyKeyUpdate replaces the shared key settings. An empty
// APIKey keeps the stored key. Models must already be validated.
type PlatformCodeBuddyKeyUpdate struct {
	APIKey              string
	InternetEnvironment string
	Models              []string
}

var (
	ErrSharedCodeBuddyKeyRequired    = errors.New("shared CodeBuddy API Key is required")
	ErrSharedCodeBuddyModelsRequired = errors.New("at least one shared CodeBuddy model is required")
)

func (r *UsersRepo) platformCodeBuddyKey() (platformCodeBuddyKeyRow, bool) {
	var row platformCodeBuddyKeyRow
	if r == nil || r.deps == nil || r.DB == nil {
		return row, false
	}
	result := r.DB.Where("id = ?", platformCodeBuddyKeyID).Limit(1).Find(&row)
	return row, result.Error == nil && result.RowsAffected > 0
}

func decodeSharedModels(raw string) []string {
	var models []string
	if json.Unmarshal([]byte(raw), &models) != nil {
		return nil
	}
	return models
}

// legacyCodeBuddySponsor returns the account named by
// AGENTFLOW_ASSISTANT_SPONSOR_LOGIN, if it exists and has a key.
func (r *UsersRepo) legacyCodeBuddySponsor() (domain.User, bool) {
	login := strings.TrimSpace(os.Getenv(CodeBuddySponsorLoginEnv))
	if login == "" || r == nil {
		return domain.User{}, false
	}
	sponsor, ok := r.UserByLoginName(login)
	if !ok || strings.TrimSpace(sponsor.CodeBuddyAPIKey) == "" {
		return domain.User{}, false
	}
	return sponsor, true
}

// SharedCodeBuddyKey returns the key users without their own borrow: the one
// configured under 平台管理, else the legacy sponsor account's.
func (r *UsersRepo) SharedCodeBuddyKey() (SharedCodeBuddyKey, bool) {
	if row, ok := r.platformCodeBuddyKey(); ok {
		models := decodeSharedModels(row.Models)
		if strings.TrimSpace(row.APIKey) != "" && len(models) > 0 {
			return SharedCodeBuddyKey{APIKey: row.APIKey, InternetEnvironment: row.InternetEnvironment, Models: models}, true
		}
	}
	sponsor, ok := r.legacyCodeBuddySponsor()
	if !ok {
		return SharedCodeBuddyKey{}, false
	}
	return SharedCodeBuddyKey{APIKey: sponsor.CodeBuddyAPIKey, InternetEnvironment: sponsor.CodeBuddyInternetEnvironment, Models: []string{SponsoredCodeBuddyModel}, SponsorID: sponsor.ID}, true
}

// SharedCodeBuddyModels returns the models users without their own key may
// use, the default first, or nil when there is no shared key.
func (r *UsersRepo) SharedCodeBuddyModels() []string {
	if key, ok := r.SharedCodeBuddyKey(); ok {
		return key.Models
	}
	return nil
}

// PlatformCodeBuddyKeyStatus describes the shared key for administrators.
func (r *UsersRepo) PlatformCodeBuddyKeyStatus() PlatformCodeBuddyKeyStatus {
	status := PlatformCodeBuddyKeyStatus{Models: []string{}}
	if row, ok := r.platformCodeBuddyKey(); ok && strings.TrimSpace(row.APIKey) != "" {
		status.Configured = true
		status.InternetEnvironment = row.InternetEnvironment
		if models := decodeSharedModels(row.Models); models != nil {
			status.Models = models
		}
		status.UpdatedAt = row.Updated
		if user, ok := r.User(row.UpdatedBy); ok {
			status.UpdatedBy = user.LoginName
		}
	}
	if sponsor, ok := r.legacyCodeBuddySponsor(); ok {
		status.LegacySponsor = sponsor.LoginName
		status.LegacySponsorModel = SponsoredCodeBuddyModel
	}
	return status
}

// SetPlatformCodeBuddyKey saves the shared key settings.
func (r *UsersRepo) SetPlatformCodeBuddyKey(update PlatformCodeBuddyKeyUpdate, actorID int64) error {
	if len(update.Models) == 0 {
		return ErrSharedCodeBuddyModelsRequired
	}
	row, _ := r.platformCodeBuddyKey()
	if key := strings.TrimSpace(update.APIKey); key != "" {
		row.APIKey = key
	}
	if strings.TrimSpace(row.APIKey) == "" {
		return ErrSharedCodeBuddyKeyRequired
	}
	models, err := json.Marshal(update.Models)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	row.ID = platformCodeBuddyKeyID
	row.InternetEnvironment = update.InternetEnvironment
	row.Models = string(models)
	row.UpdatedBy = actorID
	row.Updated = &now
	return r.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"api_key", "internet_environment", "models", "updated_by", "updated_at"}),
	}).Create(&row).Error
}

// ClearPlatformCodeBuddyKey removes the shared key; users without their own
// key need one again (or fall back to the legacy sponsor account).
func (r *UsersRepo) ClearPlatformCodeBuddyKey() error {
	return r.DB.Where("id = ?", platformCodeBuddyKeyID).Delete(&platformCodeBuddyKeyRow{}).Error
}

// ==========================================
// 新增：Kimi 与 DeepSeek 共享 Key 共存逻辑
// ==========================================

// 1. 定义表结构，增加 Provider
type platformKimiKeyRow struct {
	ID        int64      `gorm:"column:id;primaryKey;autoIncrement"`
	Provider  string     `gorm:"column:provider;size:50;not null;default:'deepseek';uniqueIndex"`
	APIKey    string     `gorm:"column:api_key;type:text;not null"`
	BaseURL   string     `gorm:"column:base_url;size:255;not null;default:'https://api.moonshot.cn/v1'"`
	Models    string     `gorm:"column:models;type:text;not null"`
	UpdatedBy int64      `gorm:"column:updated_by;not null;default:0"`
	Updated   *time.Time `gorm:"column:updated_at"`
}

func (platformKimiKeyRow) TableName() string { return "platform_kimi_keys" }

// 2. 定义前端需要的状态结构体
type PlatformKimiKeyStatus struct {
	Configured bool       `json:"configured"`
	Provider   string     `json:"provider"`
	BaseURL    string     `json:"baseUrl"`
	Models     []string   `json:"models"`
	UpdatedAt  *time.Time `json:"updatedAt,omitempty"`
	UpdatedBy  string     `json:"updatedBy,omitempty"`
}

type PlatformKimiKeyUpdate struct {
	Provider string
	APIKey  string
	BaseURL string
	Models  []string
}

func normalizeSharedKeyProvider(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider != "deepseek" {
		return "kimi"
	}
	return provider
}

// 3. 内部查询方法（按 Provider 查询）
func (r *UsersRepo) platformKimiKeyByProvider(provider string) (platformKimiKeyRow, bool) {
	var row platformKimiKeyRow
	if r == nil || r.deps == nil || r.DB == nil {
		return row, false
	}
	result := r.DB.Where("provider = ?", provider).Limit(1).Find(&row)
	return row, result.Error == nil && result.RowsAffected > 0
}

// 4. 核心方法：按 Provider 获取共享 Key
func (r *UsersRepo) SharedKeyByProvider(provider string) (SharedCodeBuddyKey, bool) {
	provider = normalizeSharedKeyProvider(provider)
	row, ok := r.platformKimiKeyByProvider(provider)

	if ok {
		models := decodeSharedModels(row.Models)
		if strings.TrimSpace(row.APIKey) != "" && len(models) > 0 {
			return SharedCodeBuddyKey{
				APIKey:              row.APIKey,
				InternetEnvironment: row.BaseURL,
				Models:              models,
			}, true
		}
	}
	return SharedCodeBuddyKey{}, false
}

// 5. 兼容旧代码：如果其他地方还在调 SharedKimiKey，默认查 deepseek
func (r *UsersRepo) SharedKimiKey() (SharedCodeBuddyKey, bool) {
	return r.SharedKeyByProvider("deepseek")
}

// 6. 获取状态（默认查 kimi 的状态给管理员看）
func (r *UsersRepo) PlatformKimiKeyStatus(provider ...string) PlatformKimiKeyStatus {
	selectedProvider := "kimi"
	if len(provider) > 0 {
		selectedProvider = normalizeSharedKeyProvider(provider[0])
	}
	status := PlatformKimiKeyStatus{Provider: selectedProvider, Models: []string{}}
	if row, ok := r.platformKimiKeyByProvider(selectedProvider); ok && strings.TrimSpace(row.APIKey) != "" {
		status.Configured = true
		status.BaseURL = row.BaseURL
		if models := decodeSharedModels(row.Models); models != nil {
			status.Models = models
		}
		status.UpdatedAt = row.Updated
		if user, ok := r.User(row.UpdatedBy); ok {
			status.UpdatedBy = user.LoginName
		}
	}
	return status
}

// 7. 保存配置（智能判断 Provider 并 Upsert）
func (r *UsersRepo) SetPlatformKimiKey(update PlatformKimiKeyUpdate, actorID int64) error {
	if len(update.Models) == 0 {
		return ErrSharedCodeBuddyModelsRequired
	}
	
	provider := normalizeSharedKeyProvider(update.Provider)
	baseURL := strings.TrimSpace(update.BaseURL)
	if baseURL == "" && provider == "deepseek" {
		baseURL = "https://api.deepseek.com/v1"
	} else if baseURL == "" {
		baseURL = "https://api.moonshot.cn/v1"
	}

	row, _ := r.platformKimiKeyByProvider(provider)
	if key := strings.TrimSpace(update.APIKey); key != "" {
		row.APIKey = key
	}
	if strings.TrimSpace(row.APIKey) == "" {
		return ErrSharedCodeBuddyKeyRequired
	}
	models, err := json.Marshal(update.Models)
	if err != nil {
		return err
	}
	
	now := time.Now().UTC()
	row.Provider = provider
	row.BaseURL = baseURL
	row.Models = string(models)
	row.UpdatedBy = actorID
	row.Updated = &now

	// 存在则更新，不存在则插入
	if row.ID > 0 {
		return r.DB.Save(&row).Error
	}
	return r.DB.Create(&row).Error
}

// ClearPlatformKimiKey 删除指定 Provider 的共享 Key
func (r *UsersRepo) ClearPlatformKimiKey(provider string) error {
	return r.DB.Where("provider = ?", normalizeSharedKeyProvider(provider)).Delete(&platformKimiKeyRow{}).Error
}
