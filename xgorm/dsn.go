package xgorm

import (
	"github.com/xiaoshicae/xtow/xerror"

	"log/slog"
	"maps"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// 本文件里没有任何「DSN 脱敏」的代码，这是有意的。
//
// 脱敏的前提是把 DSN 放进了日志，然后再想办法把密码抠掉——那是个
// 永远做不干净的活：URL 形式、key=value 形式、密码里带 @ 或空格、
// 同一个 key 写两遍、解析失败的兜底……每一条都是一次可能的泄漏。
//
// 这里的做法是根本不打印 DSN。日志只写从 DSN 里解出来的、确定不含凭证的
// 结构化字段：驱动、地址、库名。没有密码进过日志，也就没有什么需要脱敏。

// resolveDSN 把配置里的超时等参数注入 DSN，并解出可安全记录的连接信息
//
// 具体怎么解由各驱动自己的 Dialect 决定，见 dialect.go
func resolveDSN(c ClientConfig) (string, ConnInfo, error) {
	d, ok := lookupDialect(c.Driver)
	if !ok {
		return "", ConnInfo{}, unknownDriver(c.Driver)
	}
	if d.Resolve == nil {
		// 没提供解析逻辑：DSN 原样用，日志里只写得出驱动名
		return c.DSN, ConnInfo{Driver: string(c.Driver)}, nil
	}
	return d.Resolve(c)
}

// unknownDriver 报「不认识的驱动」，并把已注册的列出来
//
// 只说「不认识」帮助有限：驱动名写错和忘了 import 对应的模块是两个不同的
// 问题，把实际注册了哪些列出来，两者一眼可分
func unknownDriver(name Driver) error {
	return xerror.Newf("xgorm", "config", "unknown Driver=%q, registered: %v "+
		"(drivers other than mysql / postgres live in their own module, import it to register)",
		name, registeredDrivers())
}

// openMySQL / openPostgres 内置两个驱动的 Dialector 构造函数
func openMySQL(dsn string) gorm.Dialector    { return mysql.Open(dsn) }
func openPostgres(dsn string) gorm.Dialector { return postgres.Open(dsn) }

// resolveMySQL 用驱动自己的解析器处理 DSN，DSN 里已写的超时不会被覆盖
func resolveMySQL(c ClientConfig) (string, ConnInfo, error) {
	cfg, err := mysqldriver.ParseDSN(c.DSN)
	if err != nil {
		// 不回传驱动的错误：它会把 DSN 片段带在错误信息里，而错误信息会被记下来
		return "", ConnInfo{}, xerror.Newf("xgorm", "config", "failed to parse DSN, check the format of %s (details omitted to keep credentials out of logs)", ConfigKey)
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

	return cfg.FormatDSN(), ConnInfo{Driver: "mysql", Addr: cfg.Addr, DB: cfg.DBName}, nil
}

// resolvePostgres 把超时和运行时参数注入 DSN
//
// 两种 DSN 格式都支持：postgres://... 的 URL 形式和 libpq 的 key=value 形式。
// 使用者在 DSN 里显式写了的 key 一律不覆盖——配置里的值只是默认值。
func resolvePostgres(c ClientConfig) (string, ConnInfo, error) {
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
		return "", ConnInfo{}, err
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
		return "", xerror.Newf("xgorm", "config", "failed to parse DSN, check the format of %s (details omitted to keep credentials out of logs)", ConfigKey)
	}
	// 显式 ParseQuery 而不是 u.Query()：后者会把错误吞掉，只返回解得出的那部分。
	// 于是密码里带一个字面 % （构成非法的百分号转义）时，那一项会被静默丢掉，
	// 回写之后 DSN 里就没有密码了——服务报「认证失败」，而配置文件里密码明明写着。
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		// 同样不回传原始错误：它带着出问题的那个片段，而那多半就是凭证
		return "", xerror.Newf("xgorm", "config", "failed to parse the query part of the DSN, check the format of %s "+
			"(a literal %% in a password must be written as %%25; details omitted to keep credentials out of logs)",
			ConfigKey)
	}

	for _, k := range slices.Sorted(maps.Keys(injects)) {
		if _, exists := q[k]; exists {
			continue
		}
		q.Set(k, injects[k])
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// injectPostgresKV 往 key=value 形式 DSN 补参数，已有的 key 保留。
//
// 判断「已经写过哪些 key」走 splitKV，和提取连接信息用的是同一份解析。
// 这里曾经另用一个正则扫 key=，于是密码里出现 connect_timeout= 就能骗过它：
//
//	password='a connect_timeout=99 b'   →   以为已经配过，不再注入
//
// 结果是 DialTimeout 这项配置悄悄失效——没有任何迹象。
// 同一种格式只留一套解析规则，以后改引号规则也只有一处要改。
func injectPostgresKV(dsn string, injects map[string]string) string {
	existing := map[string]struct{}{}
	for _, tok := range splitKV(dsn) {
		if k, _, ok := strings.Cut(tok, "="); ok {
			existing[k] = struct{}{}
		}
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
func postgresConnInfo(dsn string) ConnInfo {
	info := ConnInfo{Driver: "postgres"}
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

// parseKV 解 libpq 的 key=value DSN，只取用得上的几个 key
//
// 故意不取 password：这个结果是给日志用的，能取到就意味着可能被打出去。
//
// 必须按 libpq 的引号规则切，不能用 strings.Fields。密码里带空格是完全
// 合法的（写成 password='a b'），而按空白切的话，引号里的内容会被当成
// 独立的 key=value 读出来：
//
//	password='prefix host=SECRET'   →   host 被解成 SECRET
//
// 那个值随后会写进建连日志。这个包一开始就决定不打印 DSN，就是为了
// 不必做「从日志里把密码抠掉」这种永远做不干净的活——从密码里抠出一段
// 再打出去，是同一个洞换了个入口。
func parseKV(dsn string) map[string]string {
	out := map[string]string{}
	for _, tok := range splitKV(dsn) {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}
		switch k {
		case "host", "port", "dbname":
			out[k] = v
		}
	}
	return out
}

// splitKV 把 libpq 的 DSN 切成一个个 key=value，认单引号和反斜杠转义。
//
// 与 quoteKV 是一对：那边负责写出去，这边负责读回来，规则必须对得上。
func splitKV(dsn string) []string {
	var (
		out     []string
		cur     strings.Builder
		quoted  bool
		escaped bool
	)
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}

	for i := 0; i < len(dsn); i++ {
		c := dsn[i]
		switch {
		case escaped:
			cur.WriteByte(c)
			escaped = false
		case c == '\\':
			escaped = true
		case c == '\'':
			quoted = !quoted
		case !quoted && (c == ' ' || c == '\t' || c == '\n' || c == '\r'):
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	// 引号没闭合说明 DSN 本身有问题。这时候切出来的东西没有一个可信，
	// 宁可什么都不报——日志里少一个字段，好过多一段密码
	if quoted {
		return nil
	}
	flush()
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
func logConn(info ConnInfo, c ClientConfig) {
	slog.Info("xgorm connected",
		"driver", info.Driver, "addr", info.Addr, "db", info.DB,
		"max_open_conns", c.MaxOpenConns, "max_idle_conns", c.MaxIdleConns)
}
