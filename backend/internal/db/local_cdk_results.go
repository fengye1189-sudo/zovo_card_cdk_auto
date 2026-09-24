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

func migrateAutomationDeclineEmails() error {
	if DB == nil {
		return fmt.Errorf("db not ready")
	}
	columns := map[string]bool{}
	rows, err := DB.Query(`SELECT name FROM pragma_table_info('automation_card_declines')`)
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
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if !columns["email_norm"] {
		if _, err = DB.Exec(`ALTER TABLE automation_card_declines ADD COLUMN email_norm TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	tx, err := DB.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Recover known emails for authoritative declines recorded before this rule.
	if _, err = tx.Exec(`UPDATE automation_card_declines
		SET email_norm=LOWER(TRIM(COALESCE((SELECT email FROM local_cdks c WHERE c.id=automation_card_declines.local_id),'')))
		WHERE TRIM(email_norm)=''`); err != nil {
		return err
	}
	// Undo only the former one-decline queue. Closed cards cannot be restored.
	if _, err = tx.Exec(`UPDATE automation_card_lifecycle
		SET retire_state='active',retire_reason='',updated_at=strftime('%s','now')
		WHERE retire_state='queued' AND retire_reason='one_decline'`); err != nil {
		return err
	}
	// A card retires only after authoritative declines from two distinct,
	// known account emails. Repeated failures for one account do not qualify.
	if _, err = tx.Exec(`UPDATE automation_card_lifecycle
		SET retire_state='queued',retire_reason='two_distinct_email_declines',updated_at=strftime('%s','now')
		WHERE retire_state='active' AND card_id IN (
			SELECT card_id FROM automation_card_declines
			WHERE TRIM(email_norm)<>''
			GROUP BY card_id HAVING COUNT(DISTINCT email_norm)>=2
		)`); err != nil {
		return err
	}
	return tx.Commit()
}
