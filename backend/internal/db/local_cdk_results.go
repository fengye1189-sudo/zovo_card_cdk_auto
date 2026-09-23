package db

import "fmt"

func migrateLocalCDKResultColumns() error {
	if DB == nil {
		return fmt.Errorf("db not ready")
	}
	columns := map[string]bool{}
	rows, err := DB.Query(`SELECT name FROM pragma_table_info('local_cdks')`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			_ = rows.Close()
			return err
		}
		columns[name] = true
	}
	if err = rows.Close(); err != nil {
		return err
	}
	additions := []struct {
		name string
		sql  string
	}{
		{"activated_at", `ALTER TABLE local_cdks ADD COLUMN activated_at INTEGER NOT NULL DEFAULT 0`},
		{"subscription_expires_at", `ALTER TABLE local_cdks ADD COLUMN subscription_expires_at INTEGER NOT NULL DEFAULT 0`},
		{"upgrade_type", `ALTER TABLE local_cdks ADD COLUMN upgrade_type TEXT NOT NULL DEFAULT ''`},
		{"expiry_estimated", `ALTER TABLE local_cdks ADD COLUMN expiry_estimated INTEGER NOT NULL DEFAULT 0`},
		{"upstream_completion_verified_at", `ALTER TABLE local_cdks ADD COLUMN upstream_completion_verified_at INTEGER NOT NULL DEFAULT 0`},
		{"upstream_completion_source", `ALTER TABLE local_cdks ADD COLUMN upstream_completion_source TEXT NOT NULL DEFAULT ''`},
	}
	for _, addition := range additions {
		if columns[addition.name] {
			continue
		}
		if _, err = DB.Exec(addition.sql); err != nil {
			return err
		}
	}
	return nil
}
