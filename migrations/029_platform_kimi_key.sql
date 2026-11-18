-- Manual, idempotent MySQL migration for the platform's shared Kimi and
-- DeepSeek API keys. The provider column separates the two configurations.
CREATE TABLE IF NOT EXISTS platform_kimi_keys (
  id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  provider VARCHAR(50) NOT NULL DEFAULT 'deepseek',
  api_key TEXT NOT NULL,
  base_url VARCHAR(255) NOT NULL DEFAULT 'https://api.moonshot.cn/v1',
  models TEXT NOT NULL,
  updated_by BIGINT NOT NULL DEFAULT 0,
  updated_at DATETIME(3) NULL,
  UNIQUE KEY uq_platform_kimi_keys_provider (provider)
);
