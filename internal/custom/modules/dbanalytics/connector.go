package dbanalytics

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type Connector interface {
	Type() string
	Validate(ctx context.Context, cfg SourceConfig, timeout time.Duration) error
	ValidateReadOnly(ctx context.Context, cfg SourceConfig, timeout time.Duration) error
	ListSchemas(ctx context.Context, cfg SourceConfig, timeout time.Duration) ([]string, error)
	ListTables(ctx context.Context, cfg SourceConfig, schema string, timeout time.Duration) ([]TableProfile, error)
	DescribeTable(ctx context.Context, cfg SourceConfig, ref TableRef, timeout time.Duration) (*TableProfile, error)
	SampleRows(ctx context.Context, cfg SourceConfig, ref TableRef, limit int, timeout time.Duration) ([]map[string]any, error)
	QueryRows(ctx context.Context, cfg SourceConfig, ref TableRef, limit int, timeout time.Duration) ([]ColumnInfo, []map[string]any, error)
	QuoteTable(ref TableRef) string
}

func connectorFor(sourceType string) (Connector, error) {
	switch strings.ToLower(strings.TrimSpace(sourceType)) {
	case SourceTypeMySQL:
		return mysqlConnector{}, nil
	case SourceTypePostgres:
		return postgresConnector{}, nil
	default:
		return nil, fmt.Errorf("unsupported database source type: %s", sourceType)
	}
}

func openConnectorDB(ctx context.Context, sourceType string, cfg SourceConfig, timeout time.Duration) (*sql.DB, error) {
	var driverName, dsn string
	switch sourceType {
	case SourceTypeMySQL:
		driverName = "mysql"
		mysqlCfg := mysql.NewConfig()
		mysqlCfg.User = cfg.Username
		mysqlCfg.Passwd = cfg.Password
		mysqlCfg.Net = "tcp"
		mysqlCfg.Addr = fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
		mysqlCfg.DBName = cfg.Database
		mysqlCfg.ParseTime = true
		mysqlCfg.Params = map[string]string{"charset": "utf8mb4"}
		for k, v := range cfg.Params {
			mysqlCfg.Params[k] = v
		}
		dsn = mysqlCfg.FormatDSN()
	case SourceTypePostgres:
		driverName = "pgx"
		values := url.Values{}
		if cfg.SSLMode != "" {
			values.Set("sslmode", cfg.SSLMode)
		} else {
			values.Set("sslmode", "disable")
		}
		for k, v := range cfg.Params {
			values.Set(k, v)
		}
		u := url.URL{
			Scheme:   "postgres",
			User:     url.UserPassword(cfg.Username, cfg.Password),
			Host:     fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
			Path:     cfg.Database,
			RawQuery: values.Encode(),
		}
		dsn = u.String()
	default:
		return nil, fmt.Errorf("unsupported database source type: %s", sourceType)
	}

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(2 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

type mysqlConnector struct{}

func (mysqlConnector) Type() string { return SourceTypeMySQL }

func (c mysqlConnector) Validate(ctx context.Context, cfg SourceConfig, timeout time.Duration) error {
	db, err := openConnectorDB(ctx, c.Type(), cfg, timeout)
	if err != nil {
		return err
	}
	return db.Close()
}

func (c mysqlConnector) ValidateReadOnly(ctx context.Context, cfg SourceConfig, timeout time.Duration) error {
	db, err := openConnectorDB(ctx, c.Type(), cfg, timeout)
	if err != nil {
		return err
	}
	defer db.Close()

	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	rows, err := db.QueryContext(checkCtx, `SHOW GRANTS FOR CURRENT_USER()`)
	if err != nil {
		return fmt.Errorf("无法读取 MySQL 账号授权: %w", err)
	}
	defer rows.Close()

	violations := make([]string, 0)
	for rows.Next() {
		var grant string
		if err := rows.Scan(&grant); err != nil {
			return err
		}
		violations = append(violations, mysqlGrantWriteViolations(grant)...)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return readOnlyViolationsError("MySQL", violations)
}

func (c mysqlConnector) ListSchemas(ctx context.Context, cfg SourceConfig, timeout time.Duration) ([]string, error) {
	db, err := openConnectorDB(ctx, c.Type(), cfg, timeout)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SHOW DATABASES`)
	if err != nil {
		if cfg.Database != "" {
			return []string{cfg.Database}, nil
		}
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if isMySQLSystemSchema(name) {
			continue
		}
		out = append(out, name)
	}
	if cfg.Database != "" && !containsString(out, cfg.Database) {
		out = append([]string{cfg.Database}, out...)
	}
	sort.Strings(out)
	return out, rows.Err()
}

func (c mysqlConnector) ListTables(ctx context.Context, cfg SourceConfig, schema string, timeout time.Duration) ([]TableProfile, error) {
	if schema == "" {
		schema = cfg.Database
	}
	db, err := openConnectorDB(ctx, c.Type(), cfg, timeout)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	query := `
SELECT table_schema, table_name, table_type, COALESCE(table_rows, 0), COALESCE(table_comment, '')
FROM information_schema.tables
WHERE table_schema = ? AND table_type IN ('BASE TABLE', 'VIEW')
ORDER BY table_name`
	rows, err := db.QueryContext(ctx, query, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TableProfile
	for rows.Next() {
		var schemaName, tableName, tableType, comment string
		var rowEstimate int64
		if err := rows.Scan(&schemaName, &tableName, &tableType, &rowEstimate, &comment); err != nil {
			return nil, err
		}
		objType := ObjectTypeTable
		if strings.Contains(strings.ToLower(tableType), "view") {
			objType = ObjectTypeView
		}
		out = append(out, TableProfile{
			SchemaName: schemaName, TableName: tableName, ObjectType: objType,
			RowEstimate: rowEstimate, Description: comment,
		})
	}
	return out, rows.Err()
}

func (c mysqlConnector) DescribeTable(ctx context.Context, cfg SourceConfig, ref TableRef, timeout time.Duration) (*TableProfile, error) {
	db, err := openConnectorDB(ctx, c.Type(), cfg, timeout)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	query := `
SELECT column_name, column_type, is_nullable, ordinal_position, COALESCE(column_comment, '')
FROM information_schema.columns
WHERE table_schema = ? AND table_name = ?
ORDER BY ordinal_position`
	rows, err := db.QueryContext(ctx, query, ref.SchemaName, ref.TableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	profile := &TableProfile{SchemaName: ref.SchemaName, TableName: ref.TableName}
	for rows.Next() {
		var name, dataType, nullable, comment string
		var ordinal int
		if err := rows.Scan(&name, &dataType, &nullable, &ordinal, &comment); err != nil {
			return nil, err
		}
		profile.Columns = append(profile.Columns, ColumnInfo{
			Name: name, DataType: dataType, Nullable: strings.EqualFold(nullable, "yes"),
			Ordinal: ordinal, Description: comment, SemanticType: inferSemanticType(name, dataType),
		})
	}
	return profile, rows.Err()
}

func (c mysqlConnector) SampleRows(ctx context.Context, cfg SourceConfig, ref TableRef, limit int, timeout time.Duration) ([]map[string]any, error) {
	_, rows, err := c.QueryRows(ctx, cfg, ref, limit, timeout)
	return rows, err
}

func (c mysqlConnector) QueryRows(ctx context.Context, cfg SourceConfig, ref TableRef, limit int, timeout time.Duration) ([]ColumnInfo, []map[string]any, error) {
	db, err := openConnectorDB(ctx, c.Type(), cfg, timeout)
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()
	query := fmt.Sprintf("SELECT * FROM %s LIMIT %d", c.QuoteTable(ref), limit)
	return scanSQLRows(ctx, db, query)
}

func (mysqlConnector) QuoteTable(ref TableRef) string {
	return fmt.Sprintf("`%s`.`%s`", strings.ReplaceAll(ref.SchemaName, "`", "``"), strings.ReplaceAll(ref.TableName, "`", "``"))
}

type postgresConnector struct{}

func (postgresConnector) Type() string { return SourceTypePostgres }

func (c postgresConnector) Validate(ctx context.Context, cfg SourceConfig, timeout time.Duration) error {
	db, err := openConnectorDB(ctx, c.Type(), cfg, timeout)
	if err != nil {
		return err
	}
	return db.Close()
}

func (c postgresConnector) ValidateReadOnly(ctx context.Context, cfg SourceConfig, timeout time.Duration) error {
	db, err := openConnectorDB(ctx, c.Type(), cfg, timeout)
	if err != nil {
		return err
	}
	defer db.Close()

	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return validatePostgresReadOnly(checkCtx, db)
}

func (c postgresConnector) ListSchemas(ctx context.Context, cfg SourceConfig, timeout time.Duration) ([]string, error) {
	db, err := openConnectorDB(ctx, c.Type(), cfg, timeout)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `
SELECT schema_name
FROM information_schema.schemata
WHERE schema_name NOT IN ('pg_catalog', 'information_schema')
  AND schema_name NOT LIKE 'pg_toast%'
ORDER BY schema_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func (c postgresConnector) ListTables(ctx context.Context, cfg SourceConfig, schema string, timeout time.Duration) ([]TableProfile, error) {
	if schema == "" {
		schema = "public"
	}
	db, err := openConnectorDB(ctx, c.Type(), cfg, timeout)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `
SELECT n.nspname,
       cls.relname,
       CASE WHEN cls.relkind = 'v' THEN 'view' ELSE 'table' END AS object_type,
       COALESCE(cls.reltuples::bigint, 0) AS row_estimate,
       COALESCE(obj_description(cls.oid), '') AS description
FROM pg_class cls
JOIN pg_namespace n ON n.oid = cls.relnamespace
WHERE n.nspname = $1 AND cls.relkind IN ('r', 'p', 'v', 'm')
ORDER BY cls.relname`, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TableProfile
	for rows.Next() {
		var item TableProfile
		if err := rows.Scan(&item.SchemaName, &item.TableName, &item.ObjectType, &item.RowEstimate, &item.Description); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (c postgresConnector) DescribeTable(ctx context.Context, cfg SourceConfig, ref TableRef, timeout time.Duration) (*TableProfile, error) {
	db, err := openConnectorDB(ctx, c.Type(), cfg, timeout)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `
SELECT a.attname,
       format_type(a.atttypid, a.atttypmod),
       NOT a.attnotnull AS nullable,
       a.attnum,
       COALESCE(col_description(a.attrelid, a.attnum), '') AS description
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum`, ref.SchemaName, ref.TableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	profile := &TableProfile{SchemaName: ref.SchemaName, TableName: ref.TableName}
	for rows.Next() {
		var col ColumnInfo
		if err := rows.Scan(&col.Name, &col.DataType, &col.Nullable, &col.Ordinal, &col.Description); err != nil {
			return nil, err
		}
		col.SemanticType = inferSemanticType(col.Name, col.DataType)
		profile.Columns = append(profile.Columns, col)
	}
	return profile, rows.Err()
}

func (c postgresConnector) SampleRows(ctx context.Context, cfg SourceConfig, ref TableRef, limit int, timeout time.Duration) ([]map[string]any, error) {
	_, rows, err := c.QueryRows(ctx, cfg, ref, limit, timeout)
	return rows, err
}

func (c postgresConnector) QueryRows(ctx context.Context, cfg SourceConfig, ref TableRef, limit int, timeout time.Duration) ([]ColumnInfo, []map[string]any, error) {
	db, err := openConnectorDB(ctx, c.Type(), cfg, timeout)
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()
	query := fmt.Sprintf("SELECT * FROM %s LIMIT %d", c.QuoteTable(ref), limit)
	return scanSQLRows(ctx, db, query)
}

func (postgresConnector) QuoteTable(ref TableRef) string {
	return fmt.Sprintf(`"%s"."%s"`, strings.ReplaceAll(ref.SchemaName, `"`, `""`), strings.ReplaceAll(ref.TableName, `"`, `""`))
}

func scanSQLRows(ctx context.Context, db *sql.DB, query string, args ...any) ([]ColumnInfo, []map[string]any, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	names, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	typesInfo, _ := rows.ColumnTypes()
	cols := make([]ColumnInfo, 0, len(names))
	for i, name := range names {
		dataType := "text"
		nullable := true
		if i < len(typesInfo) {
			if dbType := typesInfo[i].DatabaseTypeName(); dbType != "" {
				dataType = dbType
			}
			if n, ok := typesInfo[i].Nullable(); ok {
				nullable = n
			}
		}
		cols = append(cols, ColumnInfo{Name: name, DataType: dataType, Nullable: nullable, Ordinal: i + 1, SemanticType: inferSemanticType(name, dataType)})
	}
	out := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(names))
		ptrs := make([]any, len(names))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		item := make(map[string]any, len(names))
		for i, name := range names {
			item[name] = normalizeDBValue(values[i])
		}
		out = append(out, item)
	}
	return cols, out, rows.Err()
}

func normalizeDBValue(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case []byte:
		return string(t)
	case time.Time:
		return t.Format(time.RFC3339)
	default:
		return t
	}
}

func isMySQLSystemSchema(name string) bool {
	switch strings.ToLower(name) {
	case "information_schema", "mysql", "performance_schema", "sys":
		return true
	default:
		return false
	}
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func mysqlGrantWriteViolations(grant string) []string {
	grant = strings.TrimSpace(grant)
	if grant == "" {
		return nil
	}

	upper := strings.ToUpper(grant)
	if !strings.HasPrefix(upper, "GRANT ") {
		return []string{"存在无法识别的授权语句"}
	}

	violations := make([]string, 0)
	if strings.Contains(upper, " WITH GRANT OPTION") {
		violations = append(violations, "GRANT OPTION")
	}

	onIndex := strings.Index(upper, " ON ")
	if onIndex < 0 {
		violations = append(violations, "存在角色授权或无法展开的授权")
		return violations
	}

	privilegePart := strings.TrimSpace(grant[len("GRANT "):onIndex])
	allowed := map[string]bool{
		"USAGE":     true,
		"SELECT":    true,
		"SHOW VIEW": true,
	}
	for _, raw := range splitMySQLPrivilegeList(privilegePart) {
		privilege := strings.TrimSpace(raw)
		if idx := strings.Index(privilege, "("); idx >= 0 {
			privilege = strings.TrimSpace(privilege[:idx])
		}
		privilege = strings.Join(strings.Fields(strings.ToUpper(privilege)), " ")
		if privilege == "" || allowed[privilege] {
			continue
		}
		violations = append(violations, privilege)
	}
	return violations
}

func splitMySQLPrivilegeList(input string) []string {
	items := make([]string, 0)
	start := 0
	depth := 0
	for i, r := range input {
		switch r {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				items = append(items, input[start:i])
				start = i + 1
			}
		}
	}
	items = append(items, input[start:])
	return items
}

func validatePostgresReadOnly(ctx context.Context, db *sql.DB) error {
	violations := make([]string, 0)

	var roleName string
	var superUser, createDB, createRole, replication, bypassRLS bool
	if err := db.QueryRowContext(ctx, `
SELECT rolname, rolsuper, rolcreatedb, rolcreaterole, rolreplication, rolbypassrls
FROM pg_roles
WHERE rolname = current_user`).Scan(
		&roleName, &superUser, &createDB, &createRole, &replication, &bypassRLS,
	); err != nil {
		return fmt.Errorf("无法读取 PostgreSQL 角色权限: %w", err)
	}
	if superUser {
		violations = append(violations, fmt.Sprintf("角色 %s 拥有 SUPERUSER", roleName))
	}
	if createDB {
		violations = append(violations, fmt.Sprintf("角色 %s 拥有 CREATEDB", roleName))
	}
	if createRole {
		violations = append(violations, fmt.Sprintf("角色 %s 拥有 CREATEROLE", roleName))
	}
	if replication {
		violations = append(violations, fmt.Sprintf("角色 %s 拥有 REPLICATION", roleName))
	}
	if bypassRLS {
		violations = append(violations, fmt.Sprintf("角色 %s 拥有 BYPASSRLS", roleName))
	}

	var canCreateDB bool
	if err := db.QueryRowContext(ctx, `SELECT has_database_privilege(current_database(), 'CREATE')`).Scan(&canCreateDB); err != nil {
		return fmt.Errorf("无法读取 PostgreSQL 数据库权限: %w", err)
	}
	if canCreateDB {
		violations = append(violations, "当前数据库 CREATE 权限")
	}

	schemaViolations, err := collectStringRows(ctx, db, `
SELECT format('%I', n.nspname)
FROM pg_namespace n
WHERE n.nspname <> 'information_schema'
  AND n.nspname NOT LIKE 'pg_%'
  AND has_schema_privilege(n.oid, 'CREATE')
ORDER BY n.nspname
LIMIT 5`)
	if err != nil {
		return fmt.Errorf("无法读取 PostgreSQL schema 权限: %w", err)
	}
	for _, item := range schemaViolations {
		violations = append(violations, "schema "+item+" CREATE 权限")
	}

	tableViolations, err := collectStringRows(ctx, db, `
SELECT format('%I.%I (%s)', n.nspname, c.relname, concat_ws(',',
  CASE WHEN has_table_privilege(c.oid, 'INSERT') THEN 'INSERT' END,
  CASE WHEN has_table_privilege(c.oid, 'UPDATE') THEN 'UPDATE' END,
  CASE WHEN has_table_privilege(c.oid, 'DELETE') THEN 'DELETE' END,
  CASE WHEN has_table_privilege(c.oid, 'TRUNCATE') THEN 'TRUNCATE' END,
  CASE WHEN has_table_privilege(c.oid, 'REFERENCES') THEN 'REFERENCES' END,
  CASE WHEN has_table_privilege(c.oid, 'TRIGGER') THEN 'TRIGGER' END
))
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname <> 'information_schema'
  AND n.nspname NOT LIKE 'pg_%'
  AND has_schema_privilege(n.oid, 'USAGE')
  AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
  AND (
    has_table_privilege(c.oid, 'INSERT') OR
    has_table_privilege(c.oid, 'UPDATE') OR
    has_table_privilege(c.oid, 'DELETE') OR
    has_table_privilege(c.oid, 'TRUNCATE') OR
    has_table_privilege(c.oid, 'REFERENCES') OR
    has_table_privilege(c.oid, 'TRIGGER')
  )
ORDER BY n.nspname, c.relname
LIMIT 5`)
	if err != nil {
		return fmt.Errorf("无法读取 PostgreSQL 表权限: %w", err)
	}
	for _, item := range tableViolations {
		violations = append(violations, "表 "+item)
	}

	sequenceViolations, err := collectStringRows(ctx, db, `
SELECT format('%I.%I (%s)', n.nspname, c.relname, concat_ws(',',
  CASE WHEN has_sequence_privilege(c.oid, 'USAGE') THEN 'USAGE' END,
  CASE WHEN has_sequence_privilege(c.oid, 'UPDATE') THEN 'UPDATE' END
))
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname <> 'information_schema'
  AND n.nspname NOT LIKE 'pg_%'
  AND has_schema_privilege(n.oid, 'USAGE')
  AND c.relkind = 'S'
  AND (
    has_sequence_privilege(c.oid, 'USAGE') OR
    has_sequence_privilege(c.oid, 'UPDATE')
  )
ORDER BY n.nspname, c.relname
LIMIT 5`)
	if err != nil {
		return fmt.Errorf("无法读取 PostgreSQL 序列权限: %w", err)
	}
	for _, item := range sequenceViolations {
		violations = append(violations, "序列 "+item)
	}

	return readOnlyViolationsError("PostgreSQL", violations)
}

func collectStringRows(ctx context.Context, db *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]string, 0)
	for rows.Next() {
		var item string
		if err := rows.Scan(&item); err != nil {
			return nil, err
		}
		if strings.TrimSpace(item) != "" {
			out = append(out, item)
		}
	}
	return out, rows.Err()
}

func readOnlyViolationsError(engine string, violations []string) error {
	violations = uniqueStrings(violations)
	if len(violations) == 0 {
		return nil
	}

	const maxDetails = 5
	details := violations
	if len(details) > maxDetails {
		details = append([]string{}, violations[:maxDetails]...)
		details = append(details, fmt.Sprintf("等 %d 项", len(violations)))
	}
	return fmt.Errorf("%s 账号不是只读账号，检测到 %s", engine, strings.Join(details, "、"))
}

func inferSemanticType(name, dataType string) string {
	lowerName := strings.ToLower(name)
	lowerType := strings.ToLower(dataType)
	if strings.Contains(lowerType, "date") || strings.Contains(lowerType, "time") ||
		strings.HasSuffix(lowerName, "_at") || strings.Contains(lowerName, "date") || strings.Contains(lowerName, "time") {
		return "time"
	}
	if strings.Contains(lowerType, "int") || strings.Contains(lowerType, "decimal") ||
		strings.Contains(lowerType, "numeric") || strings.Contains(lowerType, "double") ||
		strings.Contains(lowerType, "float") || strings.Contains(lowerType, "real") ||
		strings.Contains(lowerType, "money") {
		if strings.HasSuffix(lowerName, "_id") || lowerName == "id" {
			return "dimension"
		}
		return "metric"
	}
	return "dimension"
}

func sourceTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		seconds = 30
	}
	return time.Duration(seconds) * time.Second
}

func intFromAny(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case int32:
		return int(n)
	case float64:
		return int(n)
	case string:
		i, _ := strconv.Atoi(n)
		return i
	default:
		return 0
	}
}
