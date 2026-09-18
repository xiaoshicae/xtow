package middleware

// 请求/响应里的敏感信息脱敏。
//
// 这里的每一条判断都必须「拿不准就当作敏感」。脱敏逻辑的失败模式是不对称的：
// 多遮一个字段只是少一点排查信息，漏遮一个就是密码进了日志、而且没人会发现。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Redacted 被遮掉的值统一写成这个
const Redacted = "***REDACTED***"

// 默认认为敏感的 body 字段名
var defaultFields = []string{
	"password", "passwd", "token", "secret", "authorization",
	"api_key", "apikey", "access_token", "refresh_token",
	"private_key", "credential",
}

// 默认认为敏感的请求头
var defaultHeaders = []string{
	"Authorization", "Cookie", "Set-Cookie", "X-Api-Key", "X-Auth-Token",
}

var (
	mu          sync.RWMutex
	extraFields []string
	extraHead   []string
	fieldSet    map[string]bool
	fieldNames  [][]byte // 小写字段名，用于 body 预检
	headerSet   map[string]bool
)

// AddSensitiveFields 追加认为敏感的 body 字段名，大小写不敏感
func AddSensitiveFields(fields ...string) {
	mu.Lock()
	defer mu.Unlock()
	extraFields = append(extraFields, fields...)
	fieldSet, fieldNames = nil, nil
}

// AddSensitiveHeaders 追加认为敏感的请求头名，大小写不敏感
func AddSensitiveHeaders(headers ...string) {
	mu.Lock()
	defer mu.Unlock()
	extraHead = append(extraHead, headers...)
	headerSet = nil
}

func fields() (map[string]bool, [][]byte) {
	mu.RLock()
	set, names := fieldSet, fieldNames
	mu.RUnlock()
	if set != nil {
		return set, names
	}

	mu.Lock()
	defer mu.Unlock()
	if fieldSet == nil {
		all := append(append([]string{}, defaultFields...), extraFields...)
		fieldSet = make(map[string]bool, len(all))
		fieldNames = make([][]byte, 0, len(all))
		for _, f := range all {
			lower := strings.ToLower(f)
			fieldSet[lower] = true
			fieldNames = append(fieldNames, []byte(lower))
		}
	}
	return fieldSet, fieldNames
}

func headers() map[string]bool {
	mu.RLock()
	set := headerSet
	mu.RUnlock()
	if set != nil {
		return set
	}

	mu.Lock()
	defer mu.Unlock()
	if headerSet == nil {
		all := append(append([]string{}, defaultHeaders...), extraHead...)
		headerSet = make(map[string]bool, len(all))
		for _, h := range all {
			headerSet[strings.ToLower(h)] = true
		}
	}
	return headerSet
}

// newlines 把换行去掉，免得一条日志被撑成好几行
var newlines = strings.NewReplacer("\r\n", "", "\r", "", "\n", "")

// RedactBody 按 Content-Type 脱敏请求体
func RedactBody(body []byte, contentType string) string {
	if len(body) == 0 {
		return ""
	}
	switch {
	case strings.Contains(contentType, "application/json"):
		return redactJSON(body)
	case strings.Contains(contentType, "x-www-form-urlencoded"):
		return redactForm(string(body))
	default:
		return newlines.Replace(string(body))
	}
}

// redactJSON 解析 JSON 并遮掉敏感字段
func redactJSON(body []byte) string {
	set, names := fields()
	if !mayContainField(body, names) {
		return newlines.Replace(string(body))
	}

	var data any
	if err := json.Unmarshal(body, &data); err != nil {
		// 解析不了就整个遮掉。这里不能原样返回：能走到这一步说明
		// 字面扫描认为里面有敏感字段名，只是我们没能力定位它
		return Redacted
	}

	redactValue(data, set)
	out, err := json.Marshal(data)
	if err != nil {
		return Redacted
	}
	return string(out)
}

// mayContainField 判断 body 里是否可能出现敏感字段名。
//
// 只在「确定不含」时返回 false——它的作用是跳过「解析 + 遍历 + 重新序列化」
// 这条重路径，而不是做安全判断，所以拿不准一律返回 true。
//
// 反斜杠是硬性的分水岭：JSON 允许在字符串里写 \uXXXX，于是
//
//	{"password": "hunter2"}
//
// 的原始字节里根本没有 password 这个子串。上一版只做字面扫描，
// 这种 body 会走快路径原样进日志——密码明文落盘，而且不会有任何迹象。
// 所以只要出现反斜杠就交给解析器，由它把转义还原成真实的 key。
func mayContainField(body []byte, names [][]byte) bool {
	if bytes.IndexByte(body, '\\') >= 0 {
		return true
	}
	for i := 0; i < len(body); i++ {
		for _, n := range names {
			if hasPrefixFold(body[i:], n) {
				return true
			}
		}
	}
	return false
}

// hasPrefixFold 判断 b 是否以 lower 开头，忽略 ASCII 大小写。
// lower 必须已经是小写的。
func hasPrefixFold(b, lower []byte) bool {
	if len(b) < len(lower) {
		return false
	}
	for i, c := range lower {
		if toLower(b[i]) != c {
			return false
		}
	}
	return true
}

// toLower 只处理 ASCII：敏感字段名都是 ASCII，不必为此走 unicode 表
func toLower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

func redactValue(v any, set map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if set[strings.ToLower(k)] {
				t[k] = Redacted
				continue
			}
			redactValue(val, set)
		}
	case []any:
		for _, item := range t {
			redactValue(item, set)
		}
	}
}

// redactForm 脱敏 form-urlencoded 请求体
//
// 用 url.ParseQuery 而不是按 & 和 = 切：表单的键是百分号编码的，
//
//	p%61ssword=hunter2
//
// 按字面切出来的键是 p%61ssword，跟 password 对不上，于是照样进日志。
// 解析一遍再比，才是跟服务端实际读到的键同一个东西。
func redactForm(body string) string {
	set, _ := fields()

	values, err := url.ParseQuery(body)
	if err != nil {
		// 解析不了就整个遮掉，理由同 JSON：定位不了就不能放行
		return Redacted
	}
	for k := range values {
		if set[strings.ToLower(k)] {
			values[k] = []string{Redacted}
		}
	}
	return values.Encode()
}

// redactedOnly 所有被遮掉的头共用这一个切片，内容恒定且只读
var redactedOnly = []string{Redacted}

// headerPool 复用脱敏用的中间 map
//
// 它唯一的用途是马上被序列化成字符串，每请求分配一次纯属浪费——
// 而一次请求要脱敏两遍（请求头 + 响应头）。
var headerPool = sync.Pool{New: func() any { return make(http.Header, 16) }}

// RedactHeaders 脱敏请求头并序列化成 JSON 字符串
//
// 不手写序列化：encoding/json 会做 HTML 转义、会按 key 排序，
// 手写极容易在这些细节上跟它产生差异，而省下的只是一次反射调用。
func RedactHeaders(h http.Header) string {
	set := headers()

	out := headerPool.Get().(http.Header)
	for k, v := range h {
		if set[strings.ToLower(k)] {
			out[k] = redactedOnly
		} else {
			out[k] = v
		}
	}

	b, err := json.Marshal(out)

	// 归还前清空：values 是对原 header 切片的引用，留着会让它们回收不掉
	clear(out)
	headerPool.Put(out)

	if err != nil {
		return "{}"
	}
	return string(b)
}
