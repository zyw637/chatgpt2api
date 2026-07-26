package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "modernc.org/sqlite"
)

type Backend interface {
	LoadAccounts() ([]map[string]any, error)
	SaveAccounts([]map[string]any) error
	LoadAuthKeys() ([]map[string]any, error)
	SaveAuthKeys([]map[string]any) error
	HealthCheck() map[string]any
	Info() map[string]any
}

type JSONDocumentBackend interface {
	LoadJSONDocument(name string) (any, error)
	SaveJSONDocument(name string, value any) error
	DeleteJSONDocument(name string) error
}

type ExternalImageTaskBackend interface {
	LoadExternalImageTasks() (map[string]map[string]any, error)
	UpsertExternalImageTask(taskKey string, task map[string]any) error
	DeleteExternalImageTasks(taskKeys []string) error
}

type LogBackend interface {
	AppendLog(item map[string]any) error
	QueryLogs(startDate, endDate string, limit int) ([]map[string]any, error)
}

type LogMaintenanceBackend interface {
	DeleteLogsBefore(day string) (int, error)
}

func NewBackendFromEnv(dataDir string) (Backend, error) {
	backendType := strings.ToLower(strings.TrimSpace(os.Getenv("STORAGE_BACKEND")))
	if backendType == "" {
		backendType = "sqlite"
	}
	switch backendType {
	case "sqlite", "postgres", "postgresql", "mysql", "database":
		dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
		if dsn == "" {
			dsn = "sqlite:///" + filepath.ToSlash(filepath.Join(dataDir, "chatgpt2api.db"))
		}
		return NewDatabaseBackend(dsn)
	default:
		return nil, fmt.Errorf("unknown storage backend: %s", backendType)
	}
}

type DatabaseBackend struct {
	databaseURL string
	driver      string
	dsn         string
	db          *sql.DB
	lockConn    *sql.Conn
	lockName    string
}

func NewDatabaseBackend(databaseURL string) (*DatabaseBackend, error) {
	driver, dsn, err := parseDatabaseURL(databaseURL)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	backend := &DatabaseBackend{databaseURL: databaseURL, driver: driver, dsn: dsn, db: db, lockName: databaseInstanceLockName(driver, dsn)}
	backend.configurePool()
	if err := backend.configureSQLite(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := backend.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := backend.acquireInstanceLock(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return backend, nil
}

func (b *DatabaseBackend) configurePool() {
	if b.driver == "sqlite" {
		// SQLite locking_mode and several PRAGMAs are connection-local. Keep the
		// only pooled connection for the backend lifetime so the instance lock
		// cannot disappear during a connection rotation.
		b.db.SetConnMaxLifetime(0)
		b.db.SetMaxOpenConns(1)
		b.db.SetMaxIdleConns(1)
		return
	}
	b.db.SetConnMaxLifetime(time.Hour)
	b.db.SetMaxOpenConns(10)
	b.db.SetMaxIdleConns(5)
}

func (b *DatabaseBackend) Close() error {
	if b == nil || b.db == nil {
		return nil
	}
	var lockErr error
	if b.lockConn != nil {
		switch b.driver {
		case "postgres":
			_, lockErr = b.lockConn.ExecContext(context.Background(), `SELECT pg_advisory_unlock(6177294722339079284)`)
		case "mysql":
			_, lockErr = b.lockConn.ExecContext(context.Background(), `SELECT RELEASE_LOCK(?)`, b.lockName)
		}
		_ = b.lockConn.Close()
		b.lockConn = nil
	}
	return errors.Join(lockErr, b.db.Close())
}

func (b *DatabaseBackend) acquireInstanceLock() error {
	if b.driver == "sqlite" {
		if _, err := b.db.Exec(`PRAGMA locking_mode=EXCLUSIVE`); err != nil {
			return fmt.Errorf("enable sqlite single-instance lock: %w", err)
		}
		if _, err := b.db.Exec(`BEGIN EXCLUSIVE`); err != nil {
			return fmt.Errorf("acquire sqlite single-instance lock: %w", err)
		}
		if _, err := b.db.Exec(`COMMIT`); err != nil {
			return fmt.Errorf("commit sqlite single-instance lock: %w", err)
		}
		return nil
	}
	conn, err := b.db.Conn(context.Background())
	if err != nil {
		return err
	}
	locked := false
	switch b.driver {
	case "postgres":
		err = conn.QueryRowContext(context.Background(), `SELECT pg_try_advisory_lock(6177294722339079284)`).Scan(&locked)
	case "mysql":
		var result sql.NullInt64
		err = conn.QueryRowContext(context.Background(), `SELECT GET_LOCK(?, 0)`, b.lockName).Scan(&result)
		locked = result.Valid && result.Int64 == 1
	default:
		err = fmt.Errorf("single-instance lock is not implemented for driver %s", b.driver)
	}
	if err != nil || !locked {
		_ = conn.Close()
		if err != nil {
			return fmt.Errorf("acquire database single-instance lock: %w", err)
		}
		return errors.New("another chatgpt2api instance is already using this database")
	}
	b.lockConn = conn
	return nil
}

func databaseInstanceLockName(driver, dsn string) string {
	identity := dsn
	if driver == "mysql" {
		if config, err := mysql.ParseDSN(dsn); err == nil {
			identity = strings.Join([]string{
				strings.ToLower(strings.TrimSpace(config.Net)),
				strings.ToLower(strings.TrimSpace(config.Addr)),
				strings.TrimSpace(config.DBName),
			}, "\x00")
		}
	}
	sum := sha256.Sum256([]byte(driver + "\x00" + identity))
	return fmt.Sprintf("chatgpt2api-%x", sum[:12])
}

func (b *DatabaseBackend) configureSQLite() error {
	if b.driver != "sqlite" {
		return nil
	}
	for _, stmt := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=NORMAL`,
		`PRAGMA busy_timeout=5000`,
		`PRAGMA temp_store=MEMORY`,
		`PRAGMA foreign_keys=ON`,
		`PRAGMA locking_mode=EXCLUSIVE`,
	} {
		if _, err := b.db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func (b *DatabaseBackend) init() error {
	schema := []string{
		`CREATE TABLE IF NOT EXISTS accounts (id INTEGER PRIMARY KEY AUTOINCREMENT, access_token TEXT UNIQUE NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS auth_keys (id INTEGER PRIMARY KEY AUTOINCREMENT, key_id TEXT UNIQUE NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS json_documents (name TEXT PRIMARY KEY, data TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS external_image_tasks (task_key TEXT PRIMARY KEY, owner_id TEXT NOT NULL, updated_at TEXT NOT NULL, data TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_external_image_tasks_owner_updated ON external_image_tasks (owner_id, updated_at)`,
		`CREATE INDEX IF NOT EXISTS idx_external_image_tasks_updated ON external_image_tasks (updated_at)`,
		`CREATE TABLE IF NOT EXISTS logs (id INTEGER PRIMARY KEY AUTOINCREMENT, created_at TEXT NOT NULL, type TEXT NOT NULL, day TEXT NOT NULL, data TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_day_id ON logs (day, id)`,
	}
	if b.driver == "postgres" {
		schema = []string{
			`CREATE TABLE IF NOT EXISTS accounts (id SERIAL PRIMARY KEY, access_token TEXT UNIQUE NOT NULL, data TEXT NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS auth_keys (id SERIAL PRIMARY KEY, key_id TEXT UNIQUE NOT NULL, data TEXT NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS json_documents (name TEXT PRIMARY KEY, data TEXT NOT NULL, updated_at TEXT NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS external_image_tasks (task_key TEXT PRIMARY KEY, owner_id TEXT NOT NULL, updated_at TEXT NOT NULL, data TEXT NOT NULL)`,
			`CREATE INDEX IF NOT EXISTS idx_external_image_tasks_owner_updated ON external_image_tasks (owner_id, updated_at)`,
			`CREATE INDEX IF NOT EXISTS idx_external_image_tasks_updated ON external_image_tasks (updated_at)`,
			`CREATE TABLE IF NOT EXISTS logs (id SERIAL PRIMARY KEY, created_at TEXT NOT NULL, type TEXT NOT NULL, day TEXT NOT NULL, data TEXT NOT NULL)`,
			`CREATE INDEX IF NOT EXISTS idx_logs_day_id ON logs (day, id)`,
		}
	}
	if b.driver == "mysql" {
		schema = []string{
			`CREATE TABLE IF NOT EXISTS accounts (id INTEGER PRIMARY KEY AUTO_INCREMENT, access_token TEXT UNIQUE NOT NULL, data TEXT NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS auth_keys (id INTEGER PRIMARY KEY AUTO_INCREMENT, key_id TEXT UNIQUE NOT NULL, data TEXT NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS json_documents (name VARCHAR(512) PRIMARY KEY, data LONGTEXT NOT NULL, updated_at TEXT NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS external_image_tasks (task_key VARCHAR(512) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin PRIMARY KEY, owner_id VARCHAR(255) NOT NULL, updated_at VARCHAR(30) NOT NULL, data LONGTEXT NOT NULL, INDEX idx_external_image_tasks_owner_updated (owner_id, updated_at), INDEX idx_external_image_tasks_updated (updated_at))`,
			`CREATE TABLE IF NOT EXISTS logs (id INTEGER PRIMARY KEY AUTO_INCREMENT, created_at TEXT NOT NULL, type VARCHAR(64) NOT NULL, day VARCHAR(10) NOT NULL, data LONGTEXT NOT NULL)`,
			`CREATE INDEX idx_logs_day_id ON logs (day, id)`,
		}
	}
	for _, stmt := range schema {
		if _, err := b.db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func (b *DatabaseBackend) LoadAccounts() ([]map[string]any, error) {
	return b.loadRows("accounts")
}

func (b *DatabaseBackend) SaveAccounts(accounts []map[string]any) error {
	return b.saveRows("accounts", "access_token", accounts)
}

func (b *DatabaseBackend) LoadAuthKeys() ([]map[string]any, error) {
	return b.loadRows("auth_keys")
}

func (b *DatabaseBackend) SaveAuthKeys(keys []map[string]any) error {
	return b.saveRows("auth_keys", "key_id", keys)
}

func (b *DatabaseBackend) HealthCheck() map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.db.PingContext(ctx); err != nil {
		return map[string]any{"status": "unhealthy", "backend": "database", "error": err.Error()}
	}
	accountCount := b.count("accounts")
	authKeyCount := b.count("auth_keys")
	documentCount := b.count("json_documents")
	logCount := b.count("logs")
	return map[string]any{"status": "healthy", "backend": "database", "database_url": maskPassword(b.databaseURL), "account_count": accountCount, "auth_key_count": authKeyCount, "document_count": documentCount, "log_count": logCount}
}

func (b *DatabaseBackend) Info() map[string]any {
	dbType := "unknown"
	switch b.driver {
	case "sqlite":
		dbType = "sqlite"
	case "postgres":
		dbType = "postgresql"
	case "mysql":
		dbType = "mysql"
	}
	return map[string]any{"type": "database", "db_type": dbType, "description": "数据库存储 (" + dbType + ")", "database_url": maskPassword(b.databaseURL)}
}

func (b *DatabaseBackend) loadRows(table string) ([]map[string]any, error) {
	rows, err := b.db.Query("SELECT data FROM " + table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, fmt.Errorf("scan %s row: %w", table, err)
		}
		var item map[string]any
		if err := json.Unmarshal([]byte(text), &item); err != nil {
			return nil, fmt.Errorf("decode %s row: %w", table, err)
		}
		if item == nil {
			return nil, fmt.Errorf("decode %s row: object is null", table)
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (b *DatabaseBackend) saveRows(table, keyColumn string, items []map[string]any) error {
	tx, err := b.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback()
	}()
	if _, err := tx.Exec("DELETE FROM " + table); err != nil {
		return err
	}
	sourceKey := "access_token"
	if table == "auth_keys" {
		sourceKey = "id"
	}
	stmtText := "INSERT INTO " + table + " (" + keyColumn + ", data) VALUES (?, ?)"
	if b.driver == "postgres" {
		stmtText = "INSERT INTO " + table + " (" + keyColumn + ", data) VALUES ($1, $2)"
	}
	stmt, err := tx.Prepare(stmtText)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, item := range items {
		key := strings.TrimSpace(fmt.Sprint(item[sourceKey]))
		if key == "" {
			continue
		}
		data, err := json.Marshal(item)
		if err != nil {
			return fmt.Errorf("encode %s row: %w", table, err)
		}
		if _, err := stmt.Exec(key, string(data)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (b *DatabaseBackend) count(table string) int {
	var count int
	_ = b.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count)
	return count
}

func (b *DatabaseBackend) LoadJSONDocument(name string) (any, error) {
	rel, err := cleanDocumentName(name)
	if err != nil {
		return nil, err
	}
	var text string
	err = b.db.QueryRow("SELECT data FROM json_documents WHERE name = "+b.placeholder(1), rel).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeJSONString(text)
}

func (b *DatabaseBackend) SaveJSONDocument(name string, value any) error {
	rel, err := cleanDocumentName(name)
	if err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var stmt string
	switch b.driver {
	case "postgres":
		stmt = "INSERT INTO json_documents (name, data, updated_at) VALUES ($1, $2, $3) ON CONFLICT (name) DO UPDATE SET data = EXCLUDED.data, updated_at = EXCLUDED.updated_at"
	case "mysql":
		stmt = "REPLACE INTO json_documents (name, data, updated_at) VALUES (?, ?, ?)"
	default:
		stmt = "INSERT INTO json_documents (name, data, updated_at) VALUES (?, ?, ?) ON CONFLICT(name) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at"
	}
	_, err = b.db.Exec(stmt, rel, string(data), now)
	return err
}

func (b *DatabaseBackend) DeleteJSONDocument(name string) error {
	rel, err := cleanDocumentName(name)
	if err != nil {
		return err
	}
	_, err = b.db.Exec("DELETE FROM json_documents WHERE name = "+b.placeholder(1), rel)
	return err
}

func (b *DatabaseBackend) LoadExternalImageTasks() (map[string]map[string]any, error) {
	rows, err := b.db.Query("SELECT task_key, data FROM external_image_tasks")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make(map[string]map[string]any)
	for rows.Next() {
		var taskKey string
		var text string
		if err := rows.Scan(&taskKey, &text); err != nil {
			return nil, fmt.Errorf("scan external image task: %w", err)
		}
		taskKey = strings.TrimSpace(taskKey)
		if taskKey == "" {
			return nil, errors.New("decode external image task: task key is empty")
		}
		value, err := decodeJSONString(text)
		if err != nil {
			return nil, fmt.Errorf("decode external image task: %w", err)
		}
		item, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("decode external image task: value is not an object")
		}
		items[taskKey] = item
	}
	return items, rows.Err()
}

func (b *DatabaseBackend) UpsertExternalImageTask(taskKey string, task map[string]any) error {
	taskKey = strings.TrimSpace(taskKey)
	if taskKey == "" {
		return errors.New("external image task key is required")
	}
	if task == nil {
		return errors.New("external image task is required")
	}
	ownerID, _ := task["owner_id"].(string)
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return errors.New("external image task owner_id is required")
	}
	updatedAt, _ := task["updated_at"].(string)
	updatedAt = strings.TrimSpace(updatedAt)
	if updatedAt == "" {
		return errors.New("external image task updated_at is required")
	}
	updatedTime, err := time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return fmt.Errorf("external image task updated_at is invalid: %w", err)
	}
	updatedAt = updatedTime.UTC().Format("2006-01-02T15:04:05.000000000Z")
	data, err := json.Marshal(task)
	if err != nil {
		return fmt.Errorf("encode external image task: %w", err)
	}
	var stmt string
	switch b.driver {
	case "postgres":
		stmt = "INSERT INTO external_image_tasks (task_key, owner_id, updated_at, data) VALUES ($1, $2, $3, $4) ON CONFLICT (task_key) DO UPDATE SET owner_id = EXCLUDED.owner_id, updated_at = EXCLUDED.updated_at, data = EXCLUDED.data"
	case "mysql":
		stmt = "INSERT INTO external_image_tasks (task_key, owner_id, updated_at, data) VALUES (?, ?, ?, ?) ON DUPLICATE KEY UPDATE owner_id = VALUES(owner_id), updated_at = VALUES(updated_at), data = VALUES(data)"
	default:
		stmt = "INSERT INTO external_image_tasks (task_key, owner_id, updated_at, data) VALUES (?, ?, ?, ?) ON CONFLICT(task_key) DO UPDATE SET owner_id = excluded.owner_id, updated_at = excluded.updated_at, data = excluded.data"
	}
	_, err = b.db.Exec(stmt, taskKey, ownerID, updatedAt, string(data))
	return err
}

func (b *DatabaseBackend) DeleteExternalImageTasks(taskKeys []string) error {
	if len(taskKeys) == 0 {
		return nil
	}
	args := make([]any, 0, len(taskKeys))
	placeholders := make([]string, 0, len(taskKeys))
	seen := make(map[string]struct{}, len(taskKeys))
	for _, taskKey := range taskKeys {
		taskKey = strings.TrimSpace(taskKey)
		if taskKey == "" {
			return errors.New("external image task key is required")
		}
		if _, ok := seen[taskKey]; ok {
			continue
		}
		seen[taskKey] = struct{}{}
		args = append(args, taskKey)
		placeholders = append(placeholders, b.placeholder(len(args)))
	}
	if len(args) == 0 {
		return nil
	}
	_, err := b.db.Exec("DELETE FROM external_image_tasks WHERE task_key IN ("+strings.Join(placeholders, ", ")+")", args...)
	return err
}

func (b *DatabaseBackend) AppendLog(item map[string]any) error {
	if item == nil {
		item = map[string]any{}
	}
	item["type"] = "event"
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	createdAt := strings.TrimSpace(fmt.Sprint(item["time"]))
	if createdAt == "" {
		createdAt = time.Now().Format("2006-01-02 15:04:05")
	}
	logType := "event"
	day := logDay(createdAt)
	if day == "" {
		day = time.Now().Format("2006-01-02")
	}
	_, err = b.db.Exec(
		"INSERT INTO logs (created_at, type, day, data) VALUES ("+b.placeholder(1)+", "+b.placeholder(2)+", "+b.placeholder(3)+", "+b.placeholder(4)+")",
		createdAt,
		logType,
		day,
		string(data),
	)
	return err
}

func (b *DatabaseBackend) QueryLogs(startDate, endDate string, limit int) ([]map[string]any, error) {
	query := "SELECT data FROM logs"
	var filters []string
	var args []any
	if strings.TrimSpace(startDate) != "" {
		args = append(args, strings.TrimSpace(startDate))
		filters = append(filters, "day >= "+b.placeholder(len(args)))
	}
	if strings.TrimSpace(endDate) != "" {
		args = append(args, strings.TrimSpace(endDate))
		filters = append(filters, "day <= "+b.placeholder(len(args)))
	}
	if len(filters) > 0 {
		query += " WHERE " + strings.Join(filters, " AND ")
	}
	query += " ORDER BY id DESC"
	if limit > 0 {
		args = append(args, limit)
		query += " LIMIT " + b.placeholder(len(args))
	}
	rows, err := b.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]map[string]any, 0)
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			continue
		}
		item, err := decodeJSONString(text)
		if err != nil {
			continue
		}
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, rows.Err()
}

func (b *DatabaseBackend) DeleteLogsBefore(day string) (int, error) {
	day = strings.TrimSpace(day)
	if day == "" {
		return 0, nil
	}
	result, err := b.db.Exec("DELETE FROM logs WHERE day < "+b.placeholder(1), day)
	if err != nil {
		return 0, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return int(rows), nil
}

func (b *DatabaseBackend) placeholder(index int) string {
	if b.driver == "postgres" {
		return fmt.Sprintf("$%d", index)
	}
	return "?"
}

func cleanDocumentName(name string) (string, error) {
	raw := strings.TrimSpace(filepath.ToSlash(name))
	rel := path.Clean(raw)
	if raw != rel || rel == "." || rel == "" || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") || strings.ContainsRune(rel, 0) || filepath.IsAbs(filepath.FromSlash(rel)) {
		return "", fmt.Errorf("invalid document name: %s", name)
	}
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." || part == ".." || strings.Contains(part, ":") {
			return "", fmt.Errorf("invalid document name: %s", name)
		}
	}
	return rel, nil
}

func decodeJSONString(text string) (any, error) {
	return decodeJSONBytes([]byte(text))
}

func decodeJSONBytes(data []byte) (any, error) {
	var out any
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return nil, fmt.Errorf("invalid trailing JSON data")
	}
	return out, nil
}

func logDay(value string) string {
	if len(value) < 10 {
		return ""
	}
	return value[:10]
}

func parseDatabaseURL(databaseURL string) (driver, dsn string, err error) {
	lower := strings.ToLower(databaseURL)
	switch {
	case strings.HasPrefix(lower, "sqlite:///"):
		return "sqlite", strings.TrimPrefix(databaseURL, "sqlite:///"), nil
	case strings.HasPrefix(lower, "sqlite://"):
		return "sqlite", strings.TrimPrefix(databaseURL, "sqlite://"), nil
	case strings.HasPrefix(lower, "postgresql://"), strings.HasPrefix(lower, "postgres://"):
		return "postgres", databaseURL, nil
	case strings.HasPrefix(lower, "mysql://"):
		u, parseErr := url.Parse(databaseURL)
		if parseErr != nil {
			return "", "", parseErr
		}
		pass, _ := u.User.Password()
		user := u.User.Username()
		db := strings.TrimPrefix(u.Path, "/")
		return "mysql", fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true", user, pass, u.Host, db), nil
	default:
		if strings.Contains(lower, "postgres") {
			return "postgres", databaseURL, nil
		}
		return "sqlite", databaseURL, nil
	}
}

func maskPassword(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	username := u.User.Username()
	if _, ok := u.User.Password(); ok {
		u.User = url.UserPassword(username, "****")
	}
	return u.String()
}
