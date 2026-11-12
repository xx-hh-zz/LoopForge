package repo

import "gorm.io/gorm/clause"

type authIntegrationRow struct {
	Provider string `gorm:"primaryKey;size:32"`
	ClientID string `gorm:"size:80"`
}

func (authIntegrationRow) TableName() string { return "auth_integrations" }

func (r *UsersRepo) GitHubClientID(fallback string) (string, error) {
	clientID, _, err := r.GitHubClientIDConfig(fallback)
	return clientID, err
}

func (r *UsersRepo) GitHubClientIDConfig(fallback string) (string, string, error) {
	var config authIntegrationRow
	result := r.DB.Where("provider = ?", "github").Limit(1).Find(&config)
	if result.Error != nil {
		return "", "", result.Error
	}
	if result.RowsAffected == 0 {
		if fallback == "" {
			return "", "disabled", nil
		}
		return fallback, "builtin", nil
	}
	if config.ClientID == "" {
		return "", "database_disabled", nil
	}
	return config.ClientID, "database", nil
}

func (r *UsersRepo) SetGitHubClientID(clientID string) error {
	return r.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "provider"}},
		DoUpdates: clause.AssignmentColumns([]string{"client_id"}),
	}).Create(&authIntegrationRow{Provider: "github", ClientID: clientID}).Error
}
