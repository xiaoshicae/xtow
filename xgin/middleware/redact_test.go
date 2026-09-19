package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const secret = "hunter2"

// mustNotLeak 断言脱敏结果里不含密码
func mustNotLeak(t *testing.T, got string) {
	t.Helper()
	if strings.Contains(got, secret) {
		t.Fatalf("密码泄漏到日志里了：%s", got)
	}
}

func TestRedactBody_JSON(t *testing.T) {
	got := RedactBody([]byte(`{"user":"alice","password":"`+secret+`"}`), "application/json")
	mustNotLeak(t, got)
	if !strings.Contains(got, `"user":"alice"`) {
		t.Errorf("非敏感字段应保留，got=%s", got)
	}
}

func TestRedactBody_JSON转义的字段名(t *testing.T) {
	// 回归用例。JSON 允许在字符串里写 \uXXXX，于是 password 可以写成
	// \u0070assword，原始字节里根本没有这个子串。
	// 上一版只做字面扫描，这种 body 会走快路径原样进日志——密码明文落盘。
	cases := map[string]string{
		"首字母转义":    `{"` + esc('p') + `assword":"` + secret + `"}`,
		"中间转义":     `{"pass` + esc('w') + `ord":"` + secret + `"}`,
		"多个字符加大小写": `{"` + esc('P') + esc('A') + `SSWORD":"` + secret + `"}`,
		"嵌套":       `{"a":{"` + esc('t') + `oken":"` + secret + `"}}`,
		"数组里":      `[{"` + esc('s') + `ecret":"` + secret + `"}]`,
	}
	for name, body := range cases {
		// 先确认这个用例真的绕过了字面扫描，否则它什么也没测到
		if strings.Contains(strings.ToLower(body), "password") ||
			strings.Contains(strings.ToLower(body), "token") ||
			strings.Contains(strings.ToLower(body), "secret") {
			t.Fatalf("%s：用例里仍然出现了字段名字面量，说明转义在传输途中被还原了：%s", name, body)
		}
		got := RedactBody([]byte(body), "application/json")
		mustNotLeak(t, got)
		if !strings.Contains(got, Redacted) {
			t.Errorf("%s 应当被遮掉，body=%s got=%s", name, body, got)
		}
	}
}

func TestRedactBody_JSON解析失败时整个遮掉(t *testing.T) {
	// 字面扫描认为里面有敏感字段名，但解析不了——定位不了就不能放行
	got := RedactBody([]byte(`{"password": "`+secret), "application/json")
	mustNotLeak(t, got)
	if got != Redacted {
		t.Errorf("解析失败应整个遮掉，got=%s", got)
	}
}

func TestRedactBody_没有敏感字段时原样保留(t *testing.T) {
	// 快路径的意义就在这里：绝大多数请求体不含敏感字段，不该为它们付解析的代价
	body := `{"user":"alice","age":30}`
	if got := RedactBody([]byte(body), "application/json"); got != body {
		t.Errorf("不含敏感字段就该原样返回，got=%s", got)
	}
}

func TestRedactBody_大小写不敏感(t *testing.T) {
	for _, body := range []string{
		`{"PASSWORD":"` + secret + `"}`,
		`{"PassWord":"` + secret + `"}`,
		`{"Api_Key":"` + secret + `"}`,
	} {
		mustNotLeak(t, RedactBody([]byte(body), "application/json"))
	}
}

func TestRedactBody_嵌套与数组(t *testing.T) {
	body := `{"a":{"b":[{"token":"` + secret + `"}]},"c":"keep"}`
	got := RedactBody([]byte(body), "application/json")
	mustNotLeak(t, got)
	if !strings.Contains(got, "keep") {
		t.Errorf("非敏感内容应保留，got=%s", got)
	}
}

func TestRedactBody_表单(t *testing.T) {
	got := RedactBody([]byte("user=alice&password="+secret), "application/x-www-form-urlencoded")
	mustNotLeak(t, got)
	if !strings.Contains(got, "user=alice") {
		t.Errorf("非敏感字段应保留，got=%s", got)
	}
}

func TestRedactBody_表单百分号编码的键(t *testing.T) {
	// 回归用例。表单的键是百分号编码的，p%61ssword 按字面切出来跟 password
	// 对不上，于是照样进日志。要跟服务端实际读到的键比，就得先解码
	for _, body := range []string{
		"p%61ssword=" + secret,
		"%50assword=" + secret,
		"%74oken=" + secret,
	} {
		got := RedactBody([]byte(body), "application/x-www-form-urlencoded")
		mustNotLeak(t, got)
		// url.Values.Encode 会把 marker 里的 * 百分号编码，先解码再比
		decoded, err := url.QueryUnescape(got)
		if err != nil {
			t.Fatalf("结果不是合法的 query：%v", err)
		}
		if !strings.Contains(decoded, Redacted) {
			t.Errorf("应当被遮掉，body=%s got=%s", body, got)
		}
	}
}

func TestRedactBody_表单解析失败时整个遮掉(t *testing.T) {
	got := RedactBody([]byte("password=%zz"), "application/x-www-form-urlencoded")
	if got != Redacted {
		t.Errorf("解析失败应整个遮掉，got=%s", got)
	}
}

func TestRedactBody_其它类型去掉换行(t *testing.T) {
	// 一条日志被撑成好几行，后面的采集和检索都会错位
	got := RedactBody([]byte("line1\nline2\r\nline3"), "text/plain")
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("应去掉换行，got=%q", got)
	}
}

func TestRedactBody_JSON的各种写法都走脱敏(t *testing.T) {
	// 之前是精确匹配 application/json，于是 +json 的各种子类型、
	// text/json、以及大写写法全都绕过了脱敏 —— 密码原样进日志。
	// Content-Type 按 RFC 9110 本就是大小写不敏感的。
	body := []byte(`{"password":"hunter2"}`)
	for _, ct := range []string{
		"application/json",
		"application/json; charset=utf-8",
		"application/vnd.api+json",
		"application/problem+json",
		"application/ld+json",
		"text/json",
		"Application/JSON",
		"APPLICATION/JSON; CHARSET=UTF-8",
	} {
		got := RedactBody(body, ct)
		if strings.Contains(got, "hunter2") {
			t.Errorf("Content-Type=%q 漏遮了密码，got=%s", ct, got)
		}
	}
}

func TestRedactBody_表单大写写法也走脱敏(t *testing.T) {
	got := RedactBody([]byte("password=hunter2"), "Application/X-WWW-Form-Urlencoded")
	if strings.Contains(got, "hunter2") {
		t.Errorf("大写的 Content-Type 同样应脱敏，got=%s", got)
	}
}

func TestRedactBody_认不出结构时有敏感字段名就整个遮掉(t *testing.T) {
	// text/plain、xml、没带 Content-Type 的 body 都定位不到具体字段，
	// 但「里面有没有敏感字段名」看得出来。有就整个遮掉 ——
	// 与 JSON 解析失败时是同一条规矩：定位不了就不能放行。
	for _, ct := range []string{"text/plain", "application/xml", "text/xml", ""} {
		got := RedactBody([]byte("<user><password>hunter2</password></user>"), ct)
		if got != Redacted {
			t.Errorf("Content-Type=%q 应整个遮掉，got=%s", ct, got)
		}
	}
}

func TestRedactBody_认不出结构且没有敏感字段名时原样保留(t *testing.T) {
	// 过度遮蔽会把排查信息一起抹掉，只在确实出现敏感字段名时才动手
	const body = "just a plain note about the weather"
	if got := RedactBody([]byte(body), "text/plain"); got != body {
		t.Errorf("没有敏感字段名就该原样保留，got=%s", got)
	}
}

func TestRedactBody_纯文本里的反斜杠不触发整体遮蔽(t *testing.T) {
	// 反斜杠是 JSON 的转义语法，纯文本里它就是个普通字符。
	// 照搬 JSON 那条「见到反斜杠一律可疑」的规矩，
	// 一条带 Windows 路径的日志会被整段遮掉
	const body = `open failed: C:\Users\app\data.txt`
	if got := RedactBody([]byte(body), "text/plain"); got != body {
		t.Errorf("纯文本里的反斜杠不该触发整体遮蔽，got=%s", got)
	}
}

func TestRedactBody_空body(t *testing.T) {
	if got := RedactBody(nil, "application/json"); got != "" {
		t.Errorf("空 body 应返回空串，got=%q", got)
	}
}

func TestAddSensitiveFields(t *testing.T) {
	reset := func() {
		mu.Lock()
		extraFields, fieldSet, fieldNames = nil, nil, nil
		mu.Unlock()
	}
	reset()
	t.Cleanup(reset)

	body := `{"my_custom_key":"` + secret + `"}`
	if got := RedactBody([]byte(body), "application/json"); strings.Contains(got, Redacted) {
		t.Fatal("还没添加就不该被遮")
	}

	AddSensitiveFields("My_Custom_Key") // 大小写不敏感
	mustNotLeak(t, RedactBody([]byte(body), "application/json"))
}

// headerLog 把脱敏后的请求头渲染成它在日志里的样子
func headerLog(h http.Header) string {
	var buf strings.Builder
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("req", "请求头", RedactHeaders(h))
	return buf.String()
}

func TestRedactHeaders(t *testing.T) {
	h := http.Header{
		"Authorization": {"Bearer " + secret},
		"Cookie":        {"session=" + secret},
		"Content-Type":  {"application/json"},
	}
	got := headerLog(h)
	mustNotLeak(t, got)
	if !strings.Contains(got, "application/json") {
		t.Errorf("非敏感头应保留，got=%s", got)
	}
	// 原 header 不能被改动
	if h.Get("Authorization") != "Bearer "+secret {
		t.Error("不该改动传进来的 header")
	}
}

func TestRedactHeaders_在日志里是嵌套对象而不是转义过的字符串(t *testing.T) {
	// 交出序列化好的字符串的话，slog 会把它当普通字符串字段再转义一遍，
	// 日志里就是 "请求头":"{\"X-A\":\"1\"}" —— 检索时要先解一层字符串
	got := headerLog(http.Header{"X-A": {"1"}})
	if strings.Contains(got, `\"`) {
		t.Errorf("不该出现双重转义，got=%s", got)
	}
	if !strings.Contains(got, `"请求头":{"X-A":"1"}`) {
		t.Errorf("应当是一个嵌套对象，got=%s", got)
	}
}

func TestRedactHeaders_多值头拼成一个字符串(t *testing.T) {
	// 同一个字段名忽而是字符串忽而是数组，日志系统建索引时会直接拒收
	got := headerLog(http.Header{"X-Multi": {"a", "b"}})
	if !strings.Contains(got, `"X-Multi":"a, b"`) {
		t.Errorf("多值应拼成一个字符串，got=%s", got)
	}
}

func TestRedactHeaders_字段按key排序(t *testing.T) {
	// map 遍历顺序是随机的，不排序的话每行日志的字段顺序都不一样
	h := http.Header{"X-C": {"3"}, "X-A": {"1"}, "X-B": {"2"}}
	for i := 0; i < 20; i++ {
		got := headerLog(h)
		if !strings.Contains(got, `{"X-A":"1","X-B":"2","X-C":"3"}`) {
			t.Fatalf("字段顺序应当稳定，got=%s", got)
		}
	}
}

func TestRedactHeaders_Cookie默认就遮(t *testing.T) {
	// Cookie 里几乎总有会话标识，等价于凭证
	mustNotLeak(t, headerLog(http.Header{"Cookie": {"sid=" + secret}}))
	mustNotLeak(t, headerLog(http.Header{"Set-Cookie": {"sid=" + secret}}))
}

func TestAddSensitiveHeaders(t *testing.T) {
	reset := func() {
		mu.Lock()
		extraHead, headerSet = nil, nil
		mu.Unlock()
	}
	reset()
	t.Cleanup(reset)

	h := http.Header{"X-Custom-Secret": {secret}}
	if !strings.Contains(headerLog(h), secret) {
		t.Fatal("还没添加就不该被遮")
	}
	AddSensitiveHeaders("x-custom-secret")
	mustNotLeak(t, headerLog(h))
}

func TestRedactHeaders_两次之间不串味(t *testing.T) {
	// 回归用例：早先的实现从池子里取一个 map 复用，忘了清空的话
	// 上一次请求的头会漏进下一条日志
	first := headerLog(http.Header{"X-One": {"1"}})
	second := headerLog(http.Header{"X-Two": {"2"}})
	if strings.Contains(second, "X-One") {
		t.Errorf("上一次请求的头漏到了这一次：first=%s second=%s", first, second)
	}
}

func TestHasPrefixFold(t *testing.T) {
	for _, c := range []struct {
		b, lower string
		want     bool
	}{
		{"PASSWORD=1", "password", true},
		{"PaSsWoRd", "password", true},
		{"passwor", "password", false}, // 比前缀短
		{"passwerd", "password", false},
		{"", "a", false},
	} {
		if got := hasPrefixFold([]byte(c.b), []byte(c.lower)); got != c.want {
			t.Errorf("hasPrefixFold(%q, %q)=%v want %v", c.b, c.lower, got, c.want)
		}
	}
}

func TestMayContainField_反斜杠一律走慢路径(t *testing.T) {
	_, names := fields()
	if !mayContainField([]byte(`{"a":"b\\c"}`), names) {
		t.Error("有反斜杠就该走慢路径——字面扫描在转义面前不可靠")
	}
	if mayContainField([]byte(`{"a":"b"}`), names) {
		t.Error("没有反斜杠也没有字段名时该走快路径")
	}
}

// 证明这组用例真的在测旧实现会漏掉的东西
func TestRedact_旧实现会漏的用例(t *testing.T) {
	escaped := `{"` + esc('p') + `assword":"` + secret + `"}`
	encoded := "p%61ssword=" + secret

	// 旧实现一：只对原始字节做字面扫描，没有反斜杠检查
	legacyJSON := func(body []byte) bool {
		_, names := fields()
		for i := 0; i < len(body); i++ {
			for _, n := range names {
				if hasPrefixFold(body[i:], n) {
					return true
				}
			}
		}
		return false
	}
	if legacyJSON([]byte(escaped)) {
		t.Fatal("这个用例对旧实现不成立，说明它没测到要测的东西")
	}

	// 旧实现二：按 & 和 = 切表单，不解码键
	legacyForm := func(body string) bool {
		set, _ := fields()
		for _, pair := range strings.Split(body, "&") {
			k, _, _ := strings.Cut(pair, "=")
			if set[strings.ToLower(k)] {
				return true
			}
		}
		return false
	}
	if legacyForm(encoded) {
		t.Fatal("这个用例对旧实现不成立，说明它没测到要测的东西")
	}

	// 同样的输入，新实现都遮住了
	mustNotLeak(t, RedactBody([]byte(escaped), "application/json"))
	mustNotLeak(t, RedactBody([]byte(encoded), "application/x-www-form-urlencoded"))
}

// esc 把一个 ASCII 字符写成 JSON 的 \uXXXX 转义形式。
//
// 不直接写字面量：这份文件要经过好几层文本传输，literal 的「反斜杠 u」
// 很容易在路上被某一层解释掉。第一版就是这么栽的——用例里的
// \u0070assword 被还原成了 password，于是整组「转义绕过」的回归用例
// 测的都是普通字段名，全部通过，却什么也没证明。
func esc(c byte) string { return fmt.Sprintf("%cu%04x", 92, c) }
