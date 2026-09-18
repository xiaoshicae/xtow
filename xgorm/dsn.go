package xgorm

import (
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
)

// 本文件里没有任何「DSN 脱敏」的代码，这是有意的。
//
// 脱敏的前提是把 DSN 放进了日志，然后再想办法把密码抠掉——那是个
// 永远做不干净的活：URL 形式、key=value 形式、密码里带 @ 或空格、
// 同一个 key 写两遍、解析失败的兜底……每一条都是一次可能的泄漏。
//
// 这里的做法是根本不打印 DSN。日志只写从 DSN 里解出来的、确定不含凭证的
// 结构化字段：驱动、地址、库名。没有密码进过日志，也就没有什么需要脱敏。

// connInfo 可以安全写进日志的连接信息
type connInfo struct {
	Driver string
	Addr   string
	DB     string
}

// resolveDSN 把配置里的超时等参数注入 DSN，并解出可安全记录的连接信息
func resolveDSN(c ClientConfig) (string, connInfo, error) {
	switch c.Driver {
	case DriverMySQL:
		return resolveMySQL(c)
	case DriverPostgres:
		return resolvePostgres(c)
	default:
		return "", connInfo{}, fmt.Errorf("不认识的 Driver=%q", c.Driver)
	}
}

// resolveMySQL 用驱动自己的解析器处理 DSN，DSN 里已写的超时不会被覆盖
func resolveMySQL(c ClientConfig) (string, connInfo, error) {
	cfg, err := mysqldriver.ParseDSN(c.DSN)
	if err != nil {
		// 不回传驱动的错误：它会把 DSN 片段带在错误信息里，而错误信息会被记下来
		return "", connInfo{}, fmt.Errorf("DSN 解析失败，检查 %s 的格式（错误详情已省略，避免凭证进日志）", ConfigKey)
	}

	if cfg.Timeout == 0 {
		cfg.Timeout = c.DialTimeout
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = c.MySQL.ReadTimeout
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = c.MySQL.WriteTimeout
	}

	return cfg.FormatDSN(), connInfo{Driver: "mysql", Addr: cfg.Addr, DB: cfg.DBName}, nil
}

// resolvePostgres 把超时和运行时参数注入 DSN
//
// 两种 DSN 格式都支持：postgres://... 的 URL 形式和 libpq 的 key=value 形式。
// 使用者在 DSN 里显式写了的 key 一律不覆盖——配置里的值只是默认值。
func resolvePostgres(c ClientConfig) (string, connInfo, error) {
	injects := map[string]string{}
	if v := seconds(c.DialTimeout); v != "" {
		injects["connect_timeout"] = v
	}
	if v := millis(c.Postgres.StatementTimeout); v != "" {
		injects["statement_timeout"] = v
	}
	if v := millis(c.Postgres.LockTimeout); v != "" {
		injects["lock_timeout"] = v
	}
	if v := millis(c.Postgres.IdleInTxTimeout); v != "" {
		injects["idle_in_transaction_session_timeout"] = v
	}
	// Params 优先于上面几个字段：同名时以使用者显式写的为准
	for k, v := range c.Postgres.Params {
		if v != "" {
			injects[k] = v
		}
	}

	dsn, err := injectPostgres(c.DSN, injects)
	if err != nil {
		return "", connInfo{}, err
	}
	return dsn, postgresConnInfo(c.DSN), nil
}

func injectPostgres(dsn string, injects map[string]string) (string, error) {
	if len(injects) == 0 {
		return dsn, nil
	}
	if isPostgresURL(dsn) {
		return injectPostgresURL(dsn, injects)
	}
	return injectPostgresKV(dsn, injects), nil
}

func isPostgresURL(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

// injectPostgresURL 往 URL 形式 DSN 的 query 里补参数，已有的 key 保留
func injectPostgresURL(dsn string, injects map[string]string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		// 同样不回传原始错误：url.Parse 的错误里带着整串 DSN
		return "", fmt.Errorf("DSN 解析失败，检查 %s 的格式（错误详情已省略，避免凭证进日志）", ConfigKey)
	}
	q := u.Query()
	for _, k := range slices.Sorted(maps.Keys(injects)) {
		if _, exists := q[k]; exists {
			continue
		}
		q.Set(k, injects[k])
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// pgKeyPattern 匹配 key=value 形式 DSN 里的 key
//
// 不处理 value 内部含 "key=" 子串的极端情况：那需要一个完整的 libpq 解析器，
// 而这里只是为了「不覆盖使用者已经写过的 key」，多注入一个参数的后果是
// 服务端报参数重复，会在启动时暴露，不是静默错误
var pgKeyPattern = regexp.MustCompile(`(?:^|\s)([a-zA-Z_][a-zA-Z0-9_]*)=`)

// injectPostgresKV 往 key=value 形式 DSN 补参数，已有的 key 保留
func injectPostgresKV(dsn string, injects map[string]string) string {
	existing := map[string]struct{}{}
	for _, m := range pgKeyPattern.FindAllStringSubmatch(dsn, -1) {
		existing[m[1]] = struct{}{}
	}

	var sb strings.Builder
	sb.WriteString(dsn)
	// 只跟末字符而不是每轮取 sb.String()：后者每次复制整串，拼 n 个参数就是 O(n²)
	needSpace := len(dsn) > 0 && !strings.HasSuffix(dsn, " ")
	for _, k := range slices.Sorted(maps.Keys(injects)) {
		if _, dup := existing[k]; dup {
			continue
		}
		if needSpace {
			sb.WriteByte(' ')
		}
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(quoteKV(injects[k]))
		needSpace = true
	}
	return sb.String()
}

// quoteKV 值里有空格、单引号或反斜杠时按 libpq 规则加引号转义
func quoteKV(v string) string {
	if v == "" {
		return "''"
	}
	if !strings.ContainsAny(v, ` '\`) {
		return v
	}
	e := strings.ReplaceAll(v, `\`, `\\`)
	e = strings.ReplaceAll(e, `'`, `\'`)
	return "'" + e + "'"
}

// postgresConnInfo 从 DSN 里取出可安全记录的部分
//
// 取不到就留空：这只是给日志用的，解析失败不该让建连失败
func postgresConnInfo(dsn string) connInfo {
	info := connInfo{Driver: "postgres"}
	if isPostgresURL(dsn) {
		if u, err := url.Parse(dsn); err == nil {
			info.Addr = u.Host
			info.DB = strings.TrimPrefix(u.Path, "/")
		}
		return info
	}

	kv := parseKV(dsn)
	host, port := kv["host"], kv["port"]
	switch {
	case host != "" && port != "":
		info.Addr = host + ":" + port
	default:
		info.Addr = host
	}
	info.DB = kv["dbname"]
	return info
}

// parseKV 粗解 libpq 的 key=value DSN，只取用得上的几个 key
//
// 故意不取 password：这个结果是给日志用的，能取到就意味着可能被打出去
func parseKV(dsn string) map[string]string {
	out := map[string]string{}
	for _, f := range strings.Fields(dsn) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		switch k {
		case "host", "port", "dbname":
			out[k] = strings.Trim(v, "'")
		}
	}
	return out
}

// seconds 向上取整为整秒，libpq 的 connect_timeout 只接受整数秒且最小为 1
func seconds(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return strconv.FormatInt(max(int64(math.Ceil(d.Seconds())), 1), 10)
}

// millis 转成毫秒整数，PG 的几个超时 GUC 用毫秒
func millis(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return strconv.FormatInt(d.Milliseconds(), 10)
}

// logConn 记一条建连日志，只写确定不含凭证的字段
func logConn(info connInfo, c ClientConfig) {
	slog.Info("xgorm 连接就绪",
		"驱动", info.Driver, "地址", info.Addr, "库", info.DB,
		"最大连接数", c.MaxOpenConns, "最大空闲", c.MaxIdleConns)
}
