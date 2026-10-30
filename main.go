package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/gjson"
	"github.com/tidwall/resp"
	"gray-whitelist-wasm/internal/whitelist"
)

type settings struct {
	IncludePathPrefixes               []string `json:"gateway_include_path_prefixes"`
	Cluster                           string   `json:"redis_cluster"`
	Database                          int      `json:"redis_database"`
	TenantKey                         string   `json:"gray_tenant_key"`
	UserKey                           string   `json:"gray_user_key"`
	EnabledKey                        string   `json:"gray_enabled_key"`
	ModeKey                           string   `json:"gray_mode_key"`
	PercentKey                        string   `json:"gray_percent_key"`
	Username                          string   `json:"redis_username"`
	Password                          string   `json:"redis_password"`
	Timeout                           int64    `json:"redis_timeout_ms"`
	Refresh                           int64    `json:"gateway_refresh_interval_seconds"`
	TTL                               int64    `json:"gateway_cache_ttl_seconds"`
	MaxEntries                        int      `json:"gray_max_entries"`
	TokenCookieName                   string   `json:"jwt_token_cookie_name"`
	TenantIDClaim                     string   `json:"jwt_tenant_id_claim"`
	UserIDClaim                       string   `json:"jwt_user_id_claim"`
	ConnectivityTestEnabled           bool     `json:"redis_connectivity_test_enabled"`
	ConnectivityTestKey               string   `json:"redis_connectivity_test_key"`
	ConnectivityTestPeriod            int64    `json:"redis_connectivity_test_interval_seconds"`
	ResponseHeaderEnabled             bool     `json:"frontend_response_header_enabled"`
	FrontendCookieName                string   `json:"frontend_gray_cookie_name"`
	FrontendCookieDomain              string   `json:"frontend_gray_cookie_domain"`
	FrontendCookieIncludePathPrefixes []string `json:"gateway_frontend_gray_cookie_include_path_prefixes"`
}

type Config struct{ runtime *runtimeState }
type runtimeState struct {
	cfg               settings
	snapshot          *graySnapshot
	refreshing        bool
	refreshGeneration uint64
	refreshDeadline   time.Time
	client            wrapper.RedisClient
	now               func() time.Time
}

func main() {}
func init() {
	wrapper.SetCtx("gray-whitelist", wrapper.ParseConfig(parseConfig),
		wrapper.ProcessRequestHeaders(onRequestHeaders),
		wrapper.ProcessResponseHeaders(onResponseHeaders))
}

func decodeSettings(raw string) (settings, error) {
	cfg := settings{
		Database: 0, TenantKey: "gray:whitelist:pc:tenantId", UserKey: "gray:whitelist:pc:userId", EnabledKey: "gray:whitelist:pc:enabled", Timeout: 1000,
		ModeKey: "gray:whitelist:pc:mode", PercentKey: "gray:whitelist:pc:percent",
		Refresh: 10, TTL: 60, MaxEntries: 10000,
		TokenCookieName: "PC_AUTH_TOKEN", TenantIDClaim: "tenantId", UserIDClaim: "id",
		ConnectivityTestKey: "gray:whitelist:pc:connectivity-test", ConnectivityTestPeriod: 60,
		ResponseHeaderEnabled:             true,
		FrontendCookieName:                "x-gray-user",
		FrontendCookieIncludePathPrefixes: []string{"/api/gray/status"},
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return cfg, errors.New("invalid configuration types")
	}
	// Reject removed names instead of silently falling back to defaults.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return cfg, err
	}
	if _, exists := fields["gateway_exclude_path_prefixes"]; exists {
		return cfg, errors.New("gateway_exclude_path_prefixes was removed in 0.7.4; remove it and use gateway_include_path_prefixes together with jwt_token_cookie_name")
	}
	for _, old := range []string{
		"redis_tenant_key", "redis_user_key", "cache_ttl_ms",
		"frontend_gray_cookie_excluded_paths",
		"response_header_enabled", "redis_connectivity_test_interval_ms",
		"gateway_cache_ttl_ms", "gateway_refresh_interval_ms",
		"max_entries", "refresh_interval_ms", "token_cookie_name", "tenant_id_claim", "user_id_claim",
		"connectivity_test_enabled", "connectivity_test_key", "connectivity_test_interval_ms",
	} {
		if _, exists := fields[old]; exists {
			return cfg, fmt.Errorf("removed configuration parameter: %s; use the new parameter name", old)
		}
	}
	if cfg.Cluster == "" || cfg.TenantKey == "" || cfg.UserKey == "" || cfg.EnabledKey == "" || cfg.ModeKey == "" || cfg.PercentKey == "" {
		return cfg, errors.New("redis_cluster and all Redis configuration keys must not be empty")
	}
	if cfg.Database < 0 || cfg.Database > 15 {
		return cfg, errors.New("redis_database must be between 0 and 15")
	}
	for _, prefix := range cfg.IncludePathPrefixes {
		if !strings.HasPrefix(prefix, "/") || strings.ContainsAny(prefix, "*?# \t\r\n") {
			return cfg, errors.New("gateway path prefixes must start with / and contain no wildcard, query, fragment or whitespace")
		}
	}
	if cfg.Timeout < 100 || cfg.Timeout > 60000 {
		return cfg, errors.New("redis_timeout_ms must be between 100 and 60000")
	}
	if cfg.Refresh < 1 || cfg.Refresh > 3600 {
		return cfg, errors.New("gateway_refresh_interval_seconds must be an integer between 1 and 3600 seconds")
	}
	if cfg.TTL < 1 || cfg.TTL > 86400 || cfg.TTL < cfg.Refresh {
		return cfg, errors.New("gateway_cache_ttl_seconds must be 1..86400 seconds and cover gateway_refresh_interval_seconds")
	}
	if cfg.MaxEntries < 1 || cfg.MaxEntries > 100000 {
		return cfg, errors.New("gray_max_entries must be between 1 and 100000")
	}
	if !validSimpleName(cfg.TokenCookieName) || !validSimpleName(cfg.TenantIDClaim) ||
		!validSimpleName(cfg.UserIDClaim) || cfg.TenantIDClaim == cfg.UserIDClaim {
		return cfg, errors.New("cookie and claim names are invalid")
	}
	if !validSimpleName(cfg.FrontendCookieName) {
		return cfg, errors.New("frontend gray cookie name is invalid")
	}
	for _, prefix := range cfg.FrontendCookieIncludePathPrefixes {
		if !validRequestPath(prefix) {
			return cfg, errors.New("gateway_frontend_gray_cookie_include_path_prefixes must contain absolute paths without query, fragment or whitespace")
		}
	}
	if cfg.ConnectivityTestEnabled {
		if cfg.ConnectivityTestKey == "" {
			return cfg, errors.New("redis_connectivity_test_key must not be empty when test is enabled")
		}
		if cfg.ConnectivityTestPeriod < 1 || cfg.ConnectivityTestPeriod > 3600 {
			return cfg, errors.New("redis_connectivity_test_interval_seconds must be an integer between 1 and 3600 seconds")
		}
	}
	return cfg, nil
}

func parseConfig(js gjson.Result, config *Config) error {
	cfg, err := decodeSettings(js.Raw)
	if err != nil {
		return err
	}
	client := wrapper.NewRedisClusterClient(wrapper.TargetCluster{Cluster: cfg.Cluster})
	if err = client.Init(cfg.Username, cfg.Password, cfg.Timeout, wrapper.WithDataBase(cfg.Database)); err != nil {
		return errors.New("redis client initialization failed")
	}
	r := &runtimeState{
		cfg: cfg, client: client, now: time.Now,
	}
	config.runtime = r
	// A separate cache per parsed rule prevents cross-rule/config-reload contamination.
	// First SDK tick initiates loading; requests never wait for Redis.
	wrapper.RegisterTickFunc(cfg.Refresh*1000, r.refresh)
	if cfg.ConnectivityTestEnabled {
		wrapper.RegisterTickFunc(cfg.ConnectivityTestPeriod*1000, r.writeConnectivityProbe)
	}
	return nil
}

// writeConnectivityProbe is an optional deployment diagnostic. It is never
// called from the request path and does not modify the whitelist Set.
func (r *runtimeState) writeConnectivityProbe() {
	err := r.client.Incr(r.cfg.ConnectivityTestKey, func(v resp.Value) {
		if v.Error() != nil || v.Type() != resp.Integer {
			proxywasm.LogWarn("gray-whitelist: Redis connectivity write test failed")
			return
		}
		proxywasm.LogInfof("gray-whitelist: Redis connectivity write test succeeded; count=%d", v.Integer())
	})
	if err != nil {
		proxywasm.LogWarn("gray-whitelist: Redis connectivity write test dispatch failed")
	}
}

func parseSnapshot(v resp.Value, max int) (map[string]struct{}, error) {
	if v.Error() != nil || v.Type() != resp.Array || v.IsNull() {
		return nil, errors.New("invalid Redis response")
	}
	if len(v.Array()) > max {
		return nil, errors.New("whitelist exceeds limit")
	}
	next := make(map[string]struct{}, len(v.Array()))
	for _, member := range v.Array() {
		if member.Type() != resp.BulkString || member.IsNull() || !whitelist.ValidSelector(member.String()) {
			return nil, errors.New("invalid whitelist selector")
		}
		next[member.String()] = struct{}{}
	}
	return next, nil
}

func parseTypedSnapshot(v resp.Value, max int, selector func(string) (string, bool)) (map[string]struct{}, error) {
	if v.Error() != nil || v.Type() != resp.Array || v.IsNull() || len(v.Array()) > max {
		return nil, errors.New("invalid Redis response")
	}
	next := make(map[string]struct{}, len(v.Array()))
	for _, member := range v.Array() {
		if member.Type() != resp.BulkString || member.IsNull() {
			return nil, errors.New("invalid whitelist member")
		}
		value, ok := selector(member.String())
		if !ok {
			return nil, errors.New("invalid whitelist member")
		}
		next[value] = struct{}{}
	}
	return next, nil
}

func onRequestHeaders(ctx wrapper.HttpContext, cfg Config) types.Action {
	ctx.SetContext("gray-request-enabled", "false")
	headers, err := proxywasm.GetHttpRequestHeaders()
	if err != nil {
		return headerFailure()
	}
	if cfg.runtime == nil || !gatewayRequestEnabled(headers, cfg.runtime.cfg) {
		return types.ActionContinue
	}
	ctx.SetContext("gray-request-enabled", "true")
	stage := cfg.runtime.stage(headers)
	output := rewriteHeadersWithStage(headers, stage)
	// Remove all client gray routing headers. Keep the original Cookie untouched.
	if err := proxywasm.ReplaceHttpRequestHeaders(output); err != nil {
		return headerFailure()
	}
	// Keep the result for the response-header callback. The client can use that
	// response value on its next request, while the current request is also
	// routed because x-gray-user was added before route recalculation.
	ctx.SetContext("gray-stage", stage)
	if frontendCookieSyncRequest(headers, cfg.runtime.cfg) {
		trackFrontendCookie(ctx, headers, cfg.runtime, stage)
	}
	return types.ActionContinue
}

func onResponseHeaders(ctx wrapper.HttpContext, cfg Config) types.Action {
	if cfg.runtime == nil || ctx.GetStringContext("gray-request-enabled", "false") != "true" {
		return types.ActionContinue
	}
	// Do not mutate redirects or error responses. Login redirects must pass
	// through untouched; the first successful authenticated API response will
	// establish or clear the frontend gray cookie.
	if !isSuccessfulResponse() {
		return types.ActionContinue
	}
	stage := ctx.GetStringContext("gray-stage", "stable")
	if cfg.runtime.cfg.ResponseHeaderEnabled {
		if err := proxywasm.ReplaceHttpResponseHeader("x-gray-user", stage); err != nil {
			proxywasm.LogWarn("gray-whitelist: cannot write response routing header")
		}
	}
	shouldUpdateCookie := ctx.GetStringContext("frontend-gray-refresh", "false") == "true"
	if shouldUpdateCookie {
		cookie := buildFrontendGrayCookie(cfg.runtime.cfg, stage)
		if err := proxywasm.AddHttpResponseHeader("Set-Cookie", cookie); err != nil {
			proxywasm.LogWarn("gray-whitelist: cannot write frontend gray cookie")
		}
		if err := proxywasm.AddHttpResponseHeader("x-frontend-gray-refresh", "true"); err != nil {
			proxywasm.LogWarn("gray-whitelist: cannot write frontend gray refresh header")
		}
	}
	return types.ActionContinue
}

func frontendCookieSyncRequest(headers [][2]string, cfg settings) bool {
	for _, h := range headers {
		if h[0] != ":path" {
			continue
		}
		path, _, _ := strings.Cut(h[1], "?")
		for _, prefix := range cfg.FrontendCookieIncludePathPrefixes {
			if strings.HasPrefix(path, prefix) {
				return true
			}
		}
		return false
	}
	return false
}

// Both conditions must hold before either gray mode or response mutation runs.
// This only checks Token presence; mode-specific validation happens in stage.
func gatewayRequestEnabled(headers [][2]string, cfg settings) bool {
	if !gatewayPathEnabled(headers, cfg) {
		return false
	}
	token, count := findCookie(headers, cfg.TokenCookieName)
	return count == 1 && token != ""
}

// Use the path visible at this hook; later route rewrites cannot change this decision.
func gatewayPathEnabled(headers [][2]string, cfg settings) bool {
	for _, h := range headers {
		if h[0] != ":path" {
			continue
		}
		path, _, _ := strings.Cut(h[1], "?")
		for _, prefix := range cfg.IncludePathPrefixes {
			if strings.HasPrefix(path, prefix) {
				return true
			}
		}
		return false
	}
	return false
}

func isSuccessfulResponse() bool {
	status, err := proxywasm.GetHttpResponseHeader(":status")
	if err != nil || status == "" {
		// Keep unit-test and hosts that do not expose :status compatible.
		return true
	}
	return len(status) == 3 && status[0] == '2' && status[1] >= '0' && status[1] <= '9' && status[2] >= '0' && status[2] <= '9'
}

func trackFrontendCookie(ctx wrapper.HttpContext, headers [][2]string, runtime *runtimeState, stage string) {
	if runtime == nil {
		return
	}
	value, count := findCookie(headers, runtime.cfg.FrontendCookieName)
	if count == 1 && value == "always" {
		ctx.SetContext("frontend-gray-cookie", "always")
	} else {
		ctx.SetContext("frontend-gray-cookie", "")
	}
	currentCookie := ctx.GetStringContext("frontend-gray-cookie", "")
	transition := (stage == "canary" && currentCookie != "always") || (stage == "stable" && currentCookie == "always")
	if transition {
		ctx.SetContext("frontend-gray-refresh", "true")
	} else {
		ctx.SetContext("frontend-gray-refresh", "false")
	}
}

func buildFrontendGrayCookie(cfg settings, stage string) string {
	if stage == "canary" {
		cookie := fmt.Sprintf("%s=always; Path=/; Secure; HttpOnly; SameSite=Lax", cfg.FrontendCookieName)
		if cfg.FrontendCookieDomain != "" {
			cookie += "; Domain=" + cfg.FrontendCookieDomain
		}
		return cookie
	}
	// Clear a previous canary cookie when the request is no longer eligible.
	cookie := fmt.Sprintf("%s=; Path=/; Max-Age=0; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Secure; HttpOnly; SameSite=Lax", cfg.FrontendCookieName)
	if cfg.FrontendCookieDomain != "" {
		cookie += "; Domain=" + cfg.FrontendCookieDomain
	}
	return cookie
}

func headerFailure() types.Action {
	proxywasm.LogError("gray-whitelist: cannot replace routing header")
	_ = proxywasm.SendHttpResponse(503, nil, []byte("Gateway header processing failed"), -1)
	return types.ActionPause
}

func resolveStage(headers [][2]string, cookieName, tenantClaim, userClaim string, contains func(string) bool) string {
	token, tokenCount := findCookie(headers, cookieName)
	stage := "stable"
	if tokenCount == 1 {
		for _, selector := range decodeJWTSelectors(token, tenantClaim, userClaim) {
			if contains(selector) {
				stage = "canary"
				break
			}
		}
	}
	return stage
}

func rewriteHeadersWithStage(headers [][2]string, stage string) [][2]string {
	output := make([][2]string, 0, len(headers)+1)
	for _, h := range headers {
		if !strings.EqualFold(h[0], "x-gray-user") {
			output = append(output, h)
		}
	}
	return append(output, [2]string{"x-gray-user", stage})
}

// rewriteHeaders is retained as a small test/helper API for callers that
// already provide the resolved stage.
func rewriteHeaders(headers [][2]string, cookieName, tenantClaim, userClaim string, contains func(string) bool) [][2]string {
	stage := resolveStage(headers, cookieName, tenantClaim, userClaim, contains)
	return rewriteHeadersWithStage(headers, stage)
}

func findCookie(headers [][2]string, name string) (string, int) {
	var value string
	count := 0
	for _, h := range headers {
		if !strings.EqualFold(h[0], "cookie") {
			continue
		}
		for _, part := range strings.Split(h[1], ";") {
			pair := strings.Trim(part, " \t")
			eq := strings.IndexByte(pair, '=')
			if eq >= 0 && pair[:eq] == name {
				value = pair[eq+1:]
				count++
			}
		}
	}
	return value, count
}

func decodeJWTSelectors(token, tenantClaim, userClaim string) []string {
	if token == "" || len(token) > 16384 {
		return nil
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !gjson.ValidBytes(payload) {
		return nil
	}
	selectors := make([]string, 0, 2)
	if tenantID, ok := claimText(payload, tenantClaim); ok {
		if selector, valid := whitelist.TenantSelector(tenantID); valid {
			selectors = append(selectors, selector)
		}
	}
	if userID, ok := claimText(payload, userClaim); ok {
		if selector, valid := whitelist.UserSelector(userID); valid {
			selectors = append(selectors, selector)
		}
	}
	return selectors
}

func claimText(payload []byte, name string) (string, bool) {
	v := gjson.GetBytes(payload, name)
	switch v.Type {
	case gjson.Number:
		return v.Raw, true
	case gjson.String:
		return v.String(), true
	default:
		return "", false
	}
}

func validSimpleName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func validRequestPath(path string) bool {
	return strings.HasPrefix(path, "/") &&
		!strings.ContainsAny(path, "*?# \t\r\n")
}
