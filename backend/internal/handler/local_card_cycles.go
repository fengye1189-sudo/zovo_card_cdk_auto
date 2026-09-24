package handler

import (
	"database/sql"

	"github.com/tuzi/cdk-recharge-system/internal/db"
)

// Card selection is lifetime-balanced and random. There is no success-count
// cap or cooldown; the stored counter is retained only for reporting.
const localCardUnlimitedLimit = 2147483647
const pro5xUsesBeforePlusPool = 3

type localCardCycle struct {
	Kind          string
	Limit         int
	Successes     int
	CycleStarted  int64
	CooldownUntil int64
}

func randomOrdinaryCardLimit() (int, error) {
	return localCardUnlimitedLimit, nil
}

// localCardKind distinguishes newly opened Pro cards from ordinary configured
// cards. Any Pro association that is not authoritatively completed remains
// isolated, even if an old settings draft still contains that card ID.
func localCardKind(cardID int64, configured bool) (string, bool, error) {
	var pro5xUses int
	err := db.DB.QueryRow("SELECT completed_uses FROM pro5x_card_policy WHERE card_id=?", cardID).Scan(&pro5xUses)
	if err == nil {
		if pro5xUses < pro5xUsesBeforePlusPool {
			return "", false, nil
		}
		return "ordinary", true, nil
	}
	if err != sql.ErrNoRows {
		return "", false, err
	}
	var state, plan, status string
	err = db.DB.QueryRow(`SELECT p.state,COALESCE(c.plan,''),COALESCE(c.status,'')
	 FROM pro_dedicated_orders p LEFT JOIN local_cdks c ON c.id=p.local_id
	 WHERE p.card_id=?`, cardID).Scan(&state, &plan, &status)
	if err == sql.ErrNoRows {
		if configured {
			return "ordinary", true, nil
		}
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if isProDedicatedPlan(plan) && status == "consumed" && (state == "submitted" || state == "completed") {
		if plan == "pro_5x" {
			return "ordinary", true, nil
		}
		return "pro", true, nil
	}
	return "", false, nil
}

func ensureLocalCardCycle(cardID int64, kind string, now int64) (localCardCycle, error) {
	limit, err := randomOrdinaryCardLimit()
	if err != nil {
		return localCardCycle{}, err
	}
	_, err = db.DB.Exec(`INSERT OR IGNORE INTO local_card_cycles
	 (card_id,card_kind,success_limit,success_count,cycle_started_at,cooldown_until,updated_at)
	 VALUES(?,?,?,0,?,0,?)`, cardID, kind, limit, now, now)
	if err != nil {
		return localCardCycle{}, err
	}
	_, err = db.DB.Exec(`INSERT OR IGNORE INTO automation_card_lifecycle(card_id,created_at,updated_at)
	 VALUES(?,?,?)`, cardID, now, now)
	if err != nil {
		return localCardCycle{}, err
	}
	_, err = db.DB.Exec(`UPDATE local_card_cycles SET card_kind=?,success_limit=?,cooldown_until=0,updated_at=?
	 WHERE card_id=?`, kind, limit, now, cardID)
	if err != nil {
		return localCardCycle{}, err
	}
	// Undo only retirements created by the removed count/cooldown policy. Exact
	// declines and manual retirement decisions remain untouched.
	_, err = db.DB.Exec(`UPDATE automation_card_lifecycle SET phase='primary',retire_state='active',retire_reason='',updated_at=?
	 WHERE card_id=? AND retire_state='queued' AND retire_reason='final_two_successes'`, now, cardID)
	if err != nil {
		return localCardCycle{}, err
	}
	var cycle localCardCycle
	err = db.DB.QueryRow(`SELECT card_kind,success_limit,success_count,cycle_started_at,cooldown_until
	 FROM local_card_cycles WHERE card_id=?`, cardID).Scan(&cycle.Kind, &cycle.Limit, &cycle.Successes, &cycle.CycleStarted, &cycle.CooldownUntil)
	return cycle, err
}

func localCardHasCycleCapacity(cardID int64, kind string, now int64) (bool, error) {
	_, err := ensureLocalCardCycle(cardID, kind, now)
	if err != nil {
		return false, err
	}
	var retireState string
	if err = db.DB.QueryRow("SELECT retire_state FROM automation_card_lifecycle WHERE card_id=?", cardID).Scan(&retireState); err != nil {
		return false, err
	}
	return retireState == "active", nil
}

// localPoolCards is the single membership rule for Plus and Go: configured
// ordinary cards plus Pro cards whose original Pro upgrade is confirmed. Go
// shares this pool but never changes its counters.
func localPoolCards(s localSettings, now int64) ([]int64, map[int64]string, error) {
	configured := map[int64]bool{}
	ordered := make([]int64, 0, len(localCardIDs(s)))
	for _, id := range localCardIDs(s) {
		if !configured[id] {
			configured[id] = true
			ordered = append(ordered, id)
		}
	}
	rows, err := db.DB.Query(`SELECT p.card_id
	 FROM pro_dedicated_orders p JOIN local_cdks c ON c.id=p.local_id
	 WHERE p.card_id>0 AND p.state IN ('submitted','completed')
	 AND c.plan IN ('pro_5x','pro_20x') AND c.status='consumed' ORDER BY p.local_id`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil && !configured[id] {
			ordered = append(ordered, id)
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, nil, err
	}
	rows, err = db.DB.Query(`SELECT card_id FROM pro5x_card_policy
	 WHERE completed_uses>=? ORDER BY updated_at,card_id`, pro5xUsesBeforePlusPool)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil && !configured[id] {
			ordered = append(ordered, id)
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, nil, err
	}
	ids := make([]int64, 0, len(ordered))
	kinds := map[int64]string{}
	for _, id := range ordered {
		kind, allowed, err := localCardKind(id, configured[id])
		if err != nil {
			return nil, nil, err
		}
		if !allowed {
			continue
		}
		capacity, err := localCardHasCycleCapacity(id, kind, now)
		if err != nil {
			return nil, nil, err
		}
		if capacity {
			ids = append(ids, id)
			kinds[id] = kind
		}
	}
	return ids, kinds, nil
}

func recordAuthoritativeLocalStatus(localID int64, state, message string, now int64) error {
	return recordAuthoritativeLocalStatusWithCompletion(localID, state, message, "", now, false)
}

// recordAuthoritativeLocalCompletion commits a verified upstream completion
// together with its local bill details and any marketplace completion event.
func recordAuthoritativeLocalCompletion(localID int64, message, completedAt string, now int64) error {
	return recordAuthoritativeLocalStatusWithCompletion(localID, "consumed", message, completedAt, now, true)
}

func recordAuthoritativeLocalStatusWithCompletion(localID int64, state, message, completedAt string, now int64, persistCompletion bool) error {
	tx, err := db.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var plan string
	var cardID int64
	var codeHash string
	err = tx.QueryRow("SELECT plan,card_id,code_hash FROM local_cdks WHERE id=?", localID).Scan(&plan, &cardID, &codeHash)
	if err != nil {
		return err
	}
	result, err := tx.Exec("UPDATE local_cdks SET status=?,message=? WHERE id=? AND status IN ('reserved','review')", state, message, localID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if state == "consumed" && persistCompletion {
		// A consumed status can also exist on historical rows that predate an
		// authoritative direct-order verification.  Persist a separate proof so
		// late marketplace binding can never turn such a row into an invoice
		// grant merely by observing its current status.
		if _, err = tx.Exec(`UPDATE local_cdks
			SET upstream_completion_verified_at=CASE WHEN upstream_completion_verified_at>0 THEN upstream_completion_verified_at ELSE ? END,
			    upstream_completion_source=CASE WHEN TRIM(upstream_completion_source)<>'' THEN upstream_completion_source ELSE 'cardplatform_direct_order' END
			WHERE id=? AND status='consumed'`, now, localID); err != nil {
			return err
		}
		activatedAt := int64(0)
		currentStatus := "consumed"
		if changed == 1 {
			if err = recordLocalCompletionDetailsTx(tx, localID, plan, completedAt, now); err != nil {
				return err
			}
			activatedAt = parseCompletionTime(completedAt, now).Unix()
		} else if err = tx.QueryRow("SELECT status,activated_at FROM local_cdks WHERE id=?", localID).Scan(&currentStatus, &activatedAt); err != nil {
			return err
		}
		if currentStatus == "consumed" && activatedAt <= 0 {
			if err = recordLocalCompletionDetailsTx(tx, localID, plan, completedAt, now); err != nil {
				return err
			}
			activatedAt = parseCompletionTime(completedAt, now).Unix()
		}
		if currentStatus == "consumed" {
			if err = enqueueMarketplaceCompletionForBoundLocalTx(tx, localID, codeHash, plan, activatedAt, now); err != nil {
				return err
			}
		}
	}
	if changed == 1 && state == "consumed" && plan == "plus" && cardID > 0 {
		kind, limit := "ordinary", localCardUnlimitedLimit
		var dedicatedPlan string
		err = tx.QueryRow(`SELECT c.plan FROM pro_dedicated_orders p JOIN local_cdks c ON c.id=p.local_id
		 WHERE p.card_id=? AND p.state IN ('submitted','completed') AND c.plan IN ('pro_5x','pro_20x') AND c.status='consumed'`, cardID).Scan(&dedicatedPlan)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if dedicatedPlan == "pro_20x" {
			kind = "pro"
		}
		if _, err = tx.Exec(`INSERT OR IGNORE INTO local_card_cycles
		 (card_id,card_kind,success_limit,success_count,cycle_started_at,cooldown_until,updated_at)
		 VALUES(?,?,?,0,?,0,?)`, cardID, kind, limit, now, now); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT OR IGNORE INTO automation_card_lifecycle(card_id,created_at,updated_at)
		 VALUES(?,?,?)`, cardID, now, now); err != nil {
			return err
		}
		var retireState string
		if err = tx.QueryRow("SELECT retire_state FROM automation_card_lifecycle WHERE card_id=?", cardID).Scan(&retireState); err != nil {
			return err
		}
		if retireState != "active" {
			return sql.ErrNoRows
		}
		result, err = tx.Exec(`UPDATE local_card_cycles SET success_count=success_count+1,
		 success_limit=?,cooldown_until=0,updated_at=? WHERE card_id=?`, localCardUnlimitedLimit, now, cardID)
		if err != nil {
			return err
		}
		counted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if counted != 1 {
			return sql.ErrNoRows
		}
	}
	if isProDedicatedPlan(plan) && (state == "consumed" || state == "failed") {
		proState := "review"
		if state == "consumed" {
			proState = "completed"
		} else if state == "failed" {
			proState = "failed"
		}
		_, err = tx.Exec("UPDATE pro_dedicated_orders SET state=? WHERE local_id=? AND state IN ('submitted','completed')", proState, localID)
		if err != nil {
			return err
		}
		if state == "consumed" && cardID > 0 {
			kind, limit := "pro", localCardUnlimitedLimit
			if plan == "pro_5x" {
				if _, err = tx.Exec(`INSERT INTO pro5x_card_policy(card_id,completed_uses,created_at,updated_at)
				 VALUES(?,1,?,?) ON CONFLICT(card_id) DO UPDATE SET
				 completed_uses=pro5x_card_policy.completed_uses+1,updated_at=excluded.updated_at`, cardID, now, now); err != nil {
					return err
				}
				var uses int
				if err = tx.QueryRow("SELECT completed_uses FROM pro5x_card_policy WHERE card_id=?", cardID).Scan(&uses); err != nil {
					return err
				}
				// Free the one-active-dedicated-order slot so the same physical card
				// can serve the next 5X order. The local order retains card_id as the
				// immutable usage history.
				if _, err = tx.Exec("UPDATE pro_dedicated_orders SET card_id=NULL WHERE local_id=? AND state='completed'", localID); err != nil {
					return err
				}
				if uses < pro5xUsesBeforePlusPool {
					return tx.Commit()
				}
				kind = "ordinary"
			}
			_, err = tx.Exec(`INSERT INTO local_card_cycles(card_id,card_kind,success_limit,success_count,cycle_started_at,cooldown_until,updated_at)
			 VALUES(?,?,?,0,?,0,?) ON CONFLICT(card_id) DO UPDATE SET
			 card_kind=excluded.card_kind,
			 success_limit=excluded.success_limit,
			 success_count=CASE WHEN local_card_cycles.card_kind=excluded.card_kind THEN local_card_cycles.success_count ELSE 0 END,
			 cycle_started_at=CASE WHEN local_card_cycles.card_kind=excluded.card_kind THEN local_card_cycles.cycle_started_at ELSE excluded.cycle_started_at END,
			 cooldown_until=0,
			 updated_at=excluded.updated_at`, cardID, kind, limit, now, now)
			if err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
