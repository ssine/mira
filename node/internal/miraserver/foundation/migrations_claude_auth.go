package foundation

const claudeAccountAuthSQL = `ALTER TABLE mira_claude_accounts
  ADD COLUMN auth_type TEXT NOT NULL DEFAULT 'api_key'
    CHECK (auth_type IN ('api_key','subscription')),
  ADD COLUMN login_email TEXT NOT NULL DEFAULT ''
    CHECK (length(login_email) <= 320);`
