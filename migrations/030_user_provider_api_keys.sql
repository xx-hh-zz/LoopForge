-- Manual MySQL migration. User queries read these optional provider keys from
-- user_configs; nullable columns preserve existing users without configured keys.
ALTER TABLE user_configs
  ADD COLUMN kimi_api_key TEXT NULL,
  ADD COLUMN deepseek_api_key TEXT NULL;
