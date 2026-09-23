package handler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mattn/go-sqlite3"
	"github.com/tuzi/cdk-recharge-system/internal/db"
)

// Online snapshots use SQLite's backup API, including committed WAL pages.
// The folder stays outside WEB_DIR; only owner-authenticated routes may download.
const operationsDailyRetention = 7
const operationsManualRetention = 3
const operationsBackupBudget int64 = 768 * 1024 * 1024
const operationsMaxSnapshotBytes int64 = 128 * 1024 * 1024

var operationsBackupName = regexp.MustCompile(`^maple-[0-9]{8}T[0-9]{6}Z-[a-f0-9]{12}\.sqlite$`)
var errOperationsBusy = errors.New("backup or verification is already running")

type operationsBackup struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	CreatedAt  int64  `json:"created_at"`
	CreatedNS  int64  `json:"created_ns,omitempty"`
	VerifiedAt int64  `json:"verified_at"`
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256"`
	Codes      int64  `json:"codes"`
	Completed  int64  `json:"completed"`
	Unresolved int64  `json:"unresolved"`
	MoneyHolds int64  `json:"money_holds"`
}

type operationsManager struct {
	database *sql.DB
	folder   string
	mu       sync.Mutex
}

var operationsService *operationsManager

func sqliteFileURI(file string, readonly bool) string {
	query := url.Values{"_busy_timeout": {"5000"}}
	if readonly {
		query.Set("mode", "ro")
		query.Set("_query_only", "1")
	} else {
		query.Set("mode", "rw")
		query.Set("_journal_mode", "DELETE")
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(file), RawQuery: query.Encode()}).String()
}

func operationsDBFile(ctx context.Context, database *sql.DB) (string, error) {
	rows, err := database.QueryContext(ctx, "PRAGMA database_list")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err != nil {
			return "", err
		}
		if name == "main" && file != "" {
			return filepath.Abs(file)
		}
	}
	return "", errors.New("file-backed SQLite database required")
}

func (m *operationsManager) prepareFolder() error {
	if err := os.MkdirAll(m.folder, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(m.folder)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("backup folder must be a regular directory")
	}
	return os.Chmod(m.folder, 0700)
}

func (m *operationsManager) file(name string) (string, error) {
	if !operationsBackupName.MatchString(name) || filepath.Base(name) != name {
		return "", errors.New("invalid backup name")
	}
	full := filepath.Join(m.folder, name)
	info, err := os.Lstat(full)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > operationsMaxSnapshotBytes {
		return "", errors.New("invalid snapshot file")
	}
	return full, nil
}

func privateNewFile(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
}

func snapshotSQLite(ctx context.Context, source *sql.DB, target string) error {
	f, err := privateNewFile(target)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	dest, err := sql.Open("sqlite3", sqliteFileURI(target, false))
	if err != nil {
		return err
	}
	defer dest.Close()
	dest.SetMaxOpenConns(1)
	src, err := source.Conn(ctx)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := dest.Conn(ctx)
	if err != nil {
		return err
	}
	defer dst.Close()
	err = dst.Raw(func(rawDest interface{}) error {
		return src.Raw(func(rawSource interface{}) error {
			d, okD := rawDest.(*sqlite3.SQLiteConn)
			s, okS := rawSource.(*sqlite3.SQLiteConn)
			if !okD || !okS {
				return errors.New("unexpected database driver")
			}
			backup, err := d.Backup("main", s, "main")
			if err != nil {
				return err
			}
			finished := false
			defer func() {
				if !finished {
					_ = backup.Finish()
				}
			}()
			for {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				done, err := backup.Step(128)
				if err != nil {
					return err
				}
				// Limit both disk use and time even if the live database keeps growing.
				if info, err := os.Stat(target); err != nil || info.Size() > operationsMaxSnapshotBytes {
					return errors.New("snapshot exceeds configured limit")
				}
				if done {
					finished = true
					return backup.Finish()
				}
				timer := time.NewTimer(10 * time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
		})
	})
	if err != nil {
		return err
	}
	return nil
}

func snapshotDigest(file string) (string, int64, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(f, operationsMaxSnapshotBytes+1))
	if err != nil || n > operationsMaxSnapshotBytes {
		return "", 0, errors.New("snapshot unreadable or too large")
	}
	return hex.EncodeToString(hash.Sum(nil)), n, nil
}

// Rehearse opening a fresh restored copy, never the live database. No credentials
// or record payloads leave this function; only counts are included in the report.
func verifySnapshot(ctx context.Context, file, folder string) (operationsBackup, error) {
	var report operationsBackup
	temp, err := os.MkdirTemp(folder, ".restore-check-")
	if err != nil {
		return report, err
	}
	defer os.RemoveAll(temp)
	in, err := os.Open(file)
	if err != nil {
		return report, err
	}
	defer in.Close()
	restored := filepath.Join(temp, "restored.sqlite")
	out, err := privateNewFile(restored)
	if err != nil {
		return report, err
	}
	n, copyErr := io.Copy(out, io.LimitReader(in, operationsMaxSnapshotBytes+1))
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil || n > operationsMaxSnapshotBytes {
		return report, errors.New("restore rehearsal copy failed")
	}
	conn, err := sql.Open("sqlite3", sqliteFileURI(restored, true))
	if err != nil {
		return report, err
	}
	defer conn.Close()
	var integrity string
	if err := conn.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		return report, errors.New("snapshot integrity check failed")
	}
	checks := []struct {
		query string
		count *int64
	}{
		{"SELECT COUNT(*) FROM local_cdks", &report.Codes},
		{"SELECT COUNT(*) FROM local_cdks WHERE status='consumed'", &report.Completed},
		{"SELECT COUNT(*) FROM local_cdks WHERE status IN ('reserved','review')", &report.Unresolved},
		{"SELECT COUNT(*) FROM automation_money WHERE state IN ('pending','inflight','unknown')", &report.MoneyHolds},
	}
	for _, check := range checks {
		if err := conn.QueryRowContext(ctx, check.query).Scan(check.count); err != nil {
			return report, errors.New("snapshot is missing required order history")
		}
	}
	for _, query := range []string{"SELECT value FROM automation_policy WHERE id=1", "SELECT value FROM notification_config WHERE id=1"} {
		var raw string
		if err := conn.QueryRowContext(ctx, query).Scan(&raw); err != nil || !json.Valid([]byte(raw)) {
			return report, errors.New("snapshot settings are invalid")
		}
	}
	var settings int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM site_settings").Scan(&settings); err != nil {
		return report, errors.New("snapshot site settings are missing")
	}
	var localSettingsJSON string
	if err := conn.QueryRowContext(ctx, "SELECT value FROM site_settings WHERE key='local_cdk_settings'").Scan(&localSettingsJSON); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return report, errors.New("snapshot redemption settings could not be read")
	} else if err == nil && !json.Valid([]byte(localSettingsJSON)) {
		return report, errors.New("snapshot redemption settings are invalid")
	}
	report.SHA256, report.Bytes, err = snapshotDigest(file)
	if err != nil {
		return report, err
	}
	report.VerifiedAt = time.Now().Unix()
	return report, nil
}

func (m *operationsManager) manifest(name string) (operationsBackup, error) {
	var report operationsBackup
	if _, err := m.file(name); err != nil {
		return report, err
	}
	file := filepath.Join(m.folder, name+".json")
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16384 {
		return report, errors.New("snapshot manifest missing")
	}
	f, err := os.Open(file)
	if err != nil {
		return report, err
	}
	defer f.Close()
	if err := json.NewDecoder(io.LimitReader(f, 16384)).Decode(&report); err != nil {
		return report, err
	}
	if report.Name != name || len(report.SHA256) != 64 || report.VerifiedAt <= 0 || (report.Kind != "daily" && report.Kind != "manual") {
		return report, errors.New("invalid snapshot manifest")
	}
	return report, nil
}

func (m *operationsManager) list() ([]operationsBackup, error) {
	list := []operationsBackup{}
	entries, err := os.ReadDir(m.folder)
	if os.IsNotExist(err) {
		return list, nil
	}
	if err != nil {
		return list, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !operationsBackupName.MatchString(entry.Name()) {
			continue
		}
		item, err := m.manifest(entry.Name())
		if err != nil {
			continue
		}
		list = append(list, item)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].CreatedAt > list[j].CreatedAt || (list[i].CreatedAt == list[j].CreatedAt && list[i].CreatedNS > list[j].CreatedNS)
	})
	return list, nil
}

func (m *operationsManager) prune() error {
	list, err := m.list()
	if err != nil {
		return err
	}
	daily, manual := 0, 0
	var bytes int64
	for _, item := range list {
		keep := false
		if item.Kind == "daily" {
			daily++
			keep = daily <= operationsDailyRetention
		} else {
			manual++
			keep = manual <= operationsManualRetention
		}
		if bytes+item.Bytes > operationsBackupBudget && bytes > 0 {
			keep = false
		}
		if keep {
			bytes += item.Bytes
			continue
		}
		file, err := m.file(item.Name)
		if err != nil {
			return err
		}
		if err := os.Remove(file); err != nil {
			return err
		}
		if err := os.Remove(file + ".json"); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (m *operationsManager) create(ctx context.Context, kind string) (operationsBackup, error) {
	var report operationsBackup
	if !m.mu.TryLock() {
		return report, errOperationsBusy
	}
	defer m.mu.Unlock()
	if kind != "daily" && kind != "manual" {
		return report, errors.New("invalid snapshot kind")
	}
	if err := m.prepareFolder(); err != nil {
		return report, err
	}
	if err := m.prune(); err != nil {
		return report, err
	}
	var pageCount, pageSize int64
	if err := m.database.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount); err != nil {
		return report, err
	}
	if err := m.database.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return report, err
	}
	if pageCount*pageSize > operationsMaxSnapshotBytes {
		return report, errors.New("database exceeds snapshot budget; move backups to larger storage")
	}
	seed := make([]byte, 6)
	if _, err := rand.Read(seed); err != nil {
		return report, err
	}
	now := time.Now().UTC()
	name := "maple-" + now.Format("20060102T150405Z") + "-" + hex.EncodeToString(seed) + ".sqlite"
	file := filepath.Join(m.folder, name)
	partial := file + ".partial"
	defer os.Remove(partial)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := snapshotSQLite(ctx, m.database, partial); err != nil {
		return report, err
	}
	report, err := verifySnapshot(ctx, partial, m.folder)
	if err != nil {
		return report, err
	}
	report.Name, report.Kind, report.CreatedAt, report.CreatedNS = name, kind, now.Unix(), now.UnixNano()
	if err := os.Rename(partial, file); err != nil {
		return report, err
	}
	meta, err := privateNewFile(file + ".json")
	if err != nil {
		os.Remove(file)
		return report, err
	}
	err = json.NewEncoder(meta).Encode(report)
	if err == nil {
		err = meta.Sync()
	}
	closeErr := meta.Close()
	if err != nil || closeErr != nil {
		os.Remove(file)
		os.Remove(file + ".json")
		return report, errors.New("snapshot manifest could not be saved")
	}
	_, _ = m.database.ExecContext(ctx, "UPDATE operations_runtime SET last_backup_success=?,last_backup_error='' WHERE id=1", now.Unix())
	if err := m.prune(); err != nil {
		operationsSetAlert("backup-retention", true, "备份保留清理失败，请检查服务器备份目录与剩余空间。")
	} else {
		operationsSetAlert("backup-retention", false, "")
	}
	return report, nil
}

func (m *operationsManager) verify(ctx context.Context, name string) (operationsBackup, error) {
	var report operationsBackup
	if !m.mu.TryLock() {
		return report, errOperationsBusy
	}
	defer m.mu.Unlock()
	saved, err := m.manifest(name)
	if err != nil {
		return report, err
	}
	file, err := m.file(name)
	if err != nil {
		return report, err
	}
	report, err = verifySnapshot(ctx, file, m.folder)
	if err != nil {
		return report, err
	}
	if report.SHA256 != saved.SHA256 || report.Bytes != saved.Bytes {
		return report, errors.New("snapshot checksum differs from manifest")
	}
	report.Name, report.Kind, report.CreatedAt, report.CreatedNS = saved.Name, saved.Kind, saved.CreatedAt, saved.CreatedNS
	return report, nil
}

func AdminOperationsBackups(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if operationsService == nil {
		localError(c, 503, "备份服务尚未启动")
		return
	}
	list, err := operationsService.list()
	if err != nil {
		localError(c, 503, "备份列表暂不可读，请检查服务器存储")
		return
	}
	c.JSON(200, gin.H{"items": list, "daily_retention": operationsDailyRetention, "manual_retention": operationsManualRetention, "storage_limit_bytes": operationsBackupBudget, "max_snapshot_bytes": operationsMaxSnapshotBytes})
}

func AdminOperationsBackupCreate(c *gin.Context) {
	if operationsService == nil {
		localError(c, 503, "备份服务尚未启动")
		return
	}
	report, err := operationsService.create(c.Request.Context(), "manual")
	if errors.Is(err, errOperationsBusy) {
		localError(c, 409, "已有备份或恢复检查正在进行，请稍后刷新")
		return
	}
	if err != nil {
		operationsBackupFailed()
		localError(c, 503, "备份未完成，请检查存储空间、数据库大小或稍后重试")
		return
	}
	operationsSetAlert("backup-failed", false, "")
	db.WriteAudit(c.GetString("username"), "backup_create", report.Name, c.ClientIP())
	c.JSON(201, report)
}

func AdminOperationsBackupVerify(c *gin.Context) {
	if operationsService == nil {
		localError(c, 503, "备份服务尚未启动")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
	defer cancel()
	report, err := operationsService.verify(ctx, c.Param("name"))
	if errors.Is(err, errOperationsBusy) {
		localError(c, 409, "已有备份或恢复检查正在进行，请稍后重试")
		return
	}
	if err != nil {
		localError(c, 422, "备份校验未通过，请创建新备份；本次没有更改线上数据")
		return
	}
	db.WriteAudit(c.GetString("username"), "backup_verify", report.Name, c.ClientIP())
	c.JSON(200, gin.H{"backup": report, "integrity": "ok", "restore_rehearsal": "passed", "production_changed": false, "message": "已从备份创建独立数据库并通过完整性、订单记录和设置检查。线上数据库未更改。"})
}

func AdminOperationsBackupDownload(c *gin.Context) {
	if operationsService == nil {
		localError(c, 503, "备份服务尚未启动")
		return
	}
	m := operationsService
	if !m.mu.TryLock() {
		localError(c, 409, "已有备份或恢复检查正在进行，请稍后下载")
		return
	}
	report, err := m.manifest(c.Param("name"))
	if err != nil {
		m.mu.Unlock()
		localError(c, 404, "备份不存在")
		return
	}
	file, err := m.file(report.Name)
	if err != nil {
		m.mu.Unlock()
		localError(c, 404, "备份不存在")
		return
	}
	checksum, size, err := snapshotDigest(file)
	if err != nil || checksum != report.SHA256 || size != report.Bytes {
		m.mu.Unlock()
		localError(c, 422, "备份校验失败，已取消下载")
		return
	}
	f, err := os.Open(file)
	m.mu.Unlock()
	if err != nil {
		localError(c, 503, "备份暂不可读")
		return
	}
	defer f.Close()
	c.Header("Cache-Control", "no-store, private")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-Backup-SHA256", report.SHA256)
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", report.Name))
	db.WriteAudit(c.GetString("username"), "backup_download", report.Name, c.ClientIP())
	c.DataFromReader(http.StatusOK, report.Bytes, "application/octet-stream", f, nil)
}

func operationsBackupFailed() {
	_, _ = db.DB.Exec("UPDATE operations_runtime SET last_backup_error='备份未完成，请检查数据库大小、存储空间和目录权限' WHERE id=1")
	operationsSetAlert("backup-failed", true, "自动备份未完成，请在运维中心检查数据库大小、存储空间及备份目录权限。")
}

func operationsBackupFolder(ctx context.Context, database *sql.DB) (string, error) {
	file, err := operationsDBFile(ctx, database)
	if err != nil {
		return "", err
	}
	folder := strings.TrimSpace(os.Getenv("OPERATIONS_BACKUP_DIR"))
	if folder == "" {
		folder = filepath.Join(filepath.Dir(file), "backups")
	}
	return filepath.Abs(folder)
}
