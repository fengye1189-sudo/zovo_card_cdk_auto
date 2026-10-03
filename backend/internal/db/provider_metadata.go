package db

import "strings"

// migrateProviderMetadata adds the provider identity used by support staff to
// distinguish locally issued cards from JZ-managed cards. It contains no
// plaintext card data and is safe to run on every startup.
func migrateProviderMetadata() error {
	for _, migration := range []string{
		`ALTER TABLE local_cdks ADD COLUMN provider TEXT NOT NULL DEFAULT 'LOCAL'`,
		`ALTER TABLE local_cdks ADD COLUMN provider_task_id TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := DB.Exec(migration); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return err
		}
	}
	_, err := DB.Exec(`CREATE INDEX IF NOT EXISTS idx_local_cdks_provider ON local_cdks(provider,provider_task_id)`)
	return err
}
