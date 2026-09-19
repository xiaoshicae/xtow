package xtrace

import (
	"context"
	"fmt"
	"maps"
	"net"
	"net/http"
	"sort"
	"strings"

	"go.opentelemetry.io/otel/propagation"
)

// context key
type (
	forwardHeadersKey struct{}
	targetHostKey     struct{}
)

// headerRule 规范化之后的域名规则
type headerRule struct {
	domains []string // 小写域名模式，支持 *.example.com
	headers []string // 规范化的 header 名
}

// HeaderPropagator 透传指定的自定义 HTTP Header（如 X-Request-Id、X-Tenant-Id）。
//
// 它实现 propagation.TextMapPropagator，所以一旦装进全局 Propagator，
// otelhttp 之类的组件会自动带上这些 Header，无需业务代码参与。
//
// 两种模式：全局透传发给所有下游；域名规则只发给匹配的下游。
type HeaderPropagator struct {
	globalHeaders []string
	rules         []headerRule
	allHeaders    []string // 全局 + 规则去重，Extract 和 Fields 用
}

// NewHeaderPropagator 创建 HeaderPropagator。
//
// header 名按 http.CanonicalHeaderKey 规范化，域名转小写。
// 域名或 header 为空的规则整条丢弃。
//
// 同一个 header 同时出现在 globalHeaders 和某条域名规则里会直接报错：
// 那是一份自相矛盾的配置——一边说发给所有人，一边说只发给这些人。
// 猜哪边为准都可能把内部标识发给第三方，所以让它在启动时就停下。
func NewHeaderPropagator(globalHeaders []string, rules []ForwardHeaderRule) (*HeaderPropagator, error) {
	normalizedRules := normalizeRules(rules)

	restricted := make(map[string]struct{})
	for _, r := range normalizedRules {
		for _, h := range r.headers {
			restricted[h] = struct{}{}
		}
	}

	global := make([]string, 0, len(globalHeaders))
	var conflicts []string
	for _, h := range canonicalHeaders(globalHeaders) {
		if _, limited := restricted[h]; limited {
			conflicts = append(conflicts, h)
			continue
		}
		global = append(global, h)
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return nil, fmt.Errorf("xtrace: header %s appears in both ForwardHeaders and ForwardHeaderRules; "+
			"the former sends it to every domain, the latter only to the listed ones — remove one of them",
			strings.Join(conflicts, ", "))
	}

	return &HeaderPropagator{
		globalHeaders: global,
		rules:         normalizedRules,
		allHeaders:    mergeHeaders(global, normalizedRules),
	}, nil
}

// normalizeRules 规范化域名规则，域名或 header 为空的规则整条丢弃
func normalizeRules(rules []ForwardHeaderRule) []headerRule {
	out := make([]headerRule, 0, len(rules))
	for _, r := range rules {
		domains := normalizeDomains(r.Domains)
		headers := canonicalHeaders(r.Headers)
		if len(domains) == 0 || len(headers) == 0 {
			continue
		}
		out = append(out, headerRule{domains: domains, headers: headers})
	}
	return out
}

// mergeHeaders 汇总去重，保持稳定顺序：全局在前，规则在后
func mergeHeaders(global []string, rules []headerRule) []string {
	all := make([]string, 0, len(global))
	seen := make(map[string]struct{}, len(global))
	add := func(h string) {
		if _, ok := seen[h]; ok {
			return
		}
		seen[h] = struct{}{}
		all = append(all, h)
	}
	for _, h := range global {
		add(h)
	}
	for _, r := range rules {
		for _, h := range r.headers {
			add(h)
		}
	}
	return all
}

// canonicalHeaders 规范化 header 名并丢弃空项
//
// 不做 TrimSpace：header 名本就不允许含空格，CanonicalHeaderKey 遇到非法字符
// 会原样返回，trim 掉反而把一个明显的配置错误悄悄改对了。
func canonicalHeaders(headers []string) []string {
	out := make([]string, 0, len(headers))
	for _, h := range headers {
		if h != "" {
			out = append(out, http.CanonicalHeaderKey(h))
		}
	}
	return out
}

func normalizeDomains(domains []string) []string {
	out := make([]string, 0, len(domains))
	for _, d := range domains {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, strings.ToLower(d))
		}
	}
	return out
}

// Extract 从上游请求里读出所有配置的 Header，存进 context。
//
// 不区分域名：进来的东西先收下，发给谁是 Inject 的事。
func (p *HeaderPropagator) Extract(ctx context.Context, carrier propagation.TextMapCarrier) context.Context {
	if len(p.allHeaders) == 0 {
		return ctx
	}

	existing := forwardHeadersRaw(ctx)
	var merged map[string]string
	for _, h := range p.allHeaders {
		v := carrier.Get(h)
		if v == "" {
			continue
		}
		if merged == nil {
			merged = make(map[string]string, len(p.allHeaders)+len(existing))
			maps.Copy(merged, existing)
		}
		merged[h] = v
	}

	if merged == nil {
		return ctx
	}
	return context.WithValue(ctx, forwardHeadersKey{}, merged)
}

// Inject 把 context 里的透传值写进下游请求。
//
// 全局 header 无条件注入；规则 header 只在目标域名匹配时注入。
// 目标域名由 Transport 写进 context——没有它，域名规则一条都不会命中。
func (p *HeaderPropagator) Inject(ctx context.Context, carrier propagation.TextMapCarrier) {
	if len(p.allHeaders) == 0 {
		return
	}
	vals := forwardHeadersRaw(ctx)
	if len(vals) == 0 {
		return
	}

	for _, h := range p.globalHeaders {
		if v := vals[h]; v != "" {
			carrier.Set(h, v)
		}
	}

	if len(p.rules) == 0 {
		return
	}
	host := TargetHostFromContext(ctx)
	if host == "" {
		return
	}
	for _, rule := range p.rules {
		if !matchDomains(host, rule.domains) {
			continue
		}
		for _, h := range rule.headers {
			if v := vals[h]; v != "" {
				carrier.Set(h, v)
			}
		}
	}
}

// Fields 返回本 Propagator 管理的全部 Header（拷贝）
func (p *HeaderPropagator) Fields() []string {
	cp := make([]string, len(p.allHeaders))
	copy(cp, p.allHeaders)
	return cp
}

// matchDomains 判断 host 是否匹配任一域名模式，支持精确匹配和 *.example.com
func matchDomains(host string, patterns []string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host // 没带端口
	}
	h = strings.ToLower(h)

	for _, pattern := range patterns {
		if suffix, ok := strings.CutPrefix(pattern, "*"); ok {
			// *.example.com 匹配 sub.example.com，但不匹配 example.com 自身
			if strings.HasSuffix(h, suffix) && len(h) > len(suffix) {
				return true
			}
			continue
		}
		if h == pattern {
			return true
		}
	}
	return false
}

// WithTargetHost 把目标请求的 Host 写进 context，供域名规则过滤使用
func WithTargetHost(ctx context.Context, host string) context.Context {
	return context.WithValue(ctx, targetHostKey{}, host)
}

// TargetHostFromContext 取出目标请求的 Host，没有则为空字符串
func TargetHostFromContext(ctx context.Context) string {
	host, _ := ctx.Value(targetHostKey{}).(string)
	return host
}

func forwardHeadersRaw(ctx context.Context) map[string]string {
	m, _ := ctx.Value(forwardHeadersKey{}).(map[string]string)
	return m
}

// ForwardHeadersFromContext 取出全部透传的 Header 键值对（拷贝）
func ForwardHeadersFromContext(ctx context.Context) map[string]string {
	m := forwardHeadersRaw(ctx)
	if len(m) == 0 {
		return nil
	}
	return maps.Clone(m)
}

// ForwardHeaderFromContext 取出指定 Header 的值，大小写不敏感
func ForwardHeaderFromContext(ctx context.Context, key string) string {
	m := forwardHeadersRaw(ctx)
	if len(m) == 0 {
		return ""
	}
	return m[http.CanonicalHeaderKey(key)]
}
