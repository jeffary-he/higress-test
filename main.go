package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
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
	Cluster                 string `json:"redis_cluster"`
	Database                int    `json:"redis_database"`
	Key                     string `json:"redis_key"`
	Username                string `json:"redis_username"`
	Password                string `json:"redis_password"`
	Timeout                 int64  `json:"redis_timeout_ms"`
	Refresh                 int64  `json:"refresh_interval_ms"`
	TTL                     int64  `json:"cache_ttl_ms"`
	MaxEntries              int    `json:"max_entries"`
	TokenCookieName         string `json:"token_cookie_name"`
	TenantIDClaim           string `json:"tenant_id_claim"`
	UserIDClaim             string `json:"user_id_claim"`
	ConnectivityTestEnabled bool   `json:"connectivity_test_enabled"`
	ConnectivityTestKey     string `json:"connectivity_test_key"`
	ConnectivityTestPeriod  int64  `json:"connectivity_test_interval_ms"`
	ResponseHeaderEnabled   bool   `json:"response_header_enabled"`
	TrustRequestHeader      bool   `json:"trust_request_header"`
}

type Config struct{ runtime *runtimeState }
type runtimeState struct {
	cfg    settings
	cache  *whitelist.Cache
	client wrapper.RedisClient
	now    func() time.Time
}

func main() {}
func init() {
	wrapper.SetCtx("gray-whitelist", wrapper.ParseConfig(parseConfig),
		wrapper.ProcessRequestHeaders(onRequestHeaders),
		wrapper.ProcessResponseHeaders(onResponseHeaders))
}

func decodeSettings(raw string) (settings, error) {
	cfg := settings{
		Database: 0, Key: "gray:whitelist:user:v1", Timeout: 1000,
		Refresh: 10000, TTL: 60000, MaxEntries: 10000,
		TokenCookieName: "PC_AUTH_TOKEN", TenantIDClaim: "tenantId", UserIDClaim: "id",
		ConnectivityTestKey: "gray:whitelist:connectivity-test:v1", ConnectivityTestPeriod: 60000,
		ResponseHeaderEnabled: false,
		TrustRequestHeader: false,
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return cfg, errors.New("invalid configuration types")
	}
	if cfg.Cluster == "" || cfg.Key == "" {
		return cfg, errors.New("redis_cluster and redis_key must not be empty")
	}
	if cfg.Database < 0 || cfg.Database > 15 {
		return cfg, errors.New("redis_database must be between 0 and 15")
	}
	if cfg.Timeout < 100 || cfg.Timeout > 60000 {
		return cfg, errors.New("redis_timeout_ms must be between 100 and 60000")
	}
	if cfg.Refresh < 1000 || cfg.Refresh > 3600000 || cfg.Refresh%100 != 0 {
		return cfg, errors.New("refresh_interval_ms must be a multiple of 100 between 1000 and 3600000")
	}
	if cfg.TTL < cfg.Refresh || cfg.TTL > 86400000 {
		return cfg, errors.New("cache_ttl_ms must be >= refresh_interval_ms and <= 86400000")
	}
	if cfg.MaxEntries < 1 || cfg.MaxEntries > 100000 {
		return cfg, errors.New("max_entries must be between 1 and 100000")
	}
	if !validSimpleName(cfg.TokenCookieName) || !validSimpleName(cfg.TenantIDClaim) ||
		!validSimpleName(cfg.UserIDClaim) || cfg.TenantIDClaim == cfg.UserIDClaim {
		return cfg, errors.New("cookie and claim names are invalid")
	}
	if cfg.ConnectivityTestEnabled {
		if cfg.ConnectivityTestKey == "" {
			return cfg, errors.New("connectivity_test_key must not be empty when test is enabled")
		}
		if cfg.ConnectivityTestPeriod < 1000 || cfg.ConnectivityTestPeriod > 3600000 || cfg.ConnectivityTestPeriod%100 != 0 {
			return cfg, errors.New("connectivity_test_interval_ms must be a multiple of 100 between 1000 and 3600000")
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
	r := &runtimeState{cfg: cfg, client: client, now: time.Now, cache: whitelist.NewCache(time.Duration(cfg.TTL) * time.Millisecond)}
	config.runtime = r
	// A separate cache per parsed rule prevents cross-rule/config-reload contamination.
	// First SDK tick initiates loading; requests never wait for Redis.
	wrapper.RegisterTickFunc(cfg.Refresh, r.refresh)
	if cfg.ConnectivityTestEnabled {
		wrapper.RegisterTickFunc(cfg.ConnectivityTestPeriod, r.writeConnectivityProbe)
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

func (r *runtimeState) refresh() {
	started := r.now()
	id, ok := r.cache.Begin(started, time.Duration(r.cfg.Timeout)*time.Millisecond)
	if !ok {
		return
	}
	err := r.client.SMembers(r.cfg.Key, func(v resp.Value) {
		next, err := parseSnapshot(v, r.cfg.MaxEntries)
		if err != nil {
			r.cache.Finish(id, started, r.now(), nil, false)
			proxywasm.LogWarn("gray-whitelist: refresh rejected; retaining previous snapshot until TTL")
			return
		}
		if r.cache.Finish(id, started, r.now(), next, true) {
			proxywasm.LogInfof("gray-whitelist: refresh succeeded; entries=%d", len(next))
		}
	})
	if err != nil {
		r.cache.Finish(id, started, r.now(), nil, false)
		proxywasm.LogWarn("gray-whitelist: Redis dispatch failed; retrying on next refresh")
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

func onRequestHeaders(ctx wrapper.HttpContext, cfg Config) types.Action {
	headers, err := proxywasm.GetHttpRequestHeaders()
	if err != nil {
		return headerFailure()
	}
	cookieName, tenantClaim, userClaim := "PC_AUTH_TOKEN", "tenantId", "id"
	if cfg.runtime != nil {
		cookieName = cfg.runtime.cfg.TokenCookieName
		tenantClaim = cfg.runtime.cfg.TenantIDClaim
		userClaim = cfg.runtime.cfg.UserIDClaim
	}
	stage := resolveStage(headers, cfg.runtime != nil && cfg.runtime.cfg.TrustRequestHeader, cookieName, tenantClaim, userClaim, func(identity string) bool {
		return cfg.runtime != nil && cfg.runtime.cache.Contains(identity, cfg.runtime.now())
	})
	output := rewriteHeadersWithStage(headers, stage)
	// Remove all client gray routing headers. Keep the original Cookie untouched.
	if err := proxywasm.ReplaceHttpRequestHeaders(output); err != nil {
		return headerFailure()
	}
	// Keep the result for the response-header callback. The client can use that
	// response value on its next request, while the current request is also
	// routed because x-gray-user was added before route recalculation.
	ctx.SetContext("gray-stage", stage)
	return types.ActionContinue
}

func onResponseHeaders(ctx wrapper.HttpContext, cfg Config) types.Action {
	if cfg.runtime == nil || !cfg.runtime.cfg.ResponseHeaderEnabled {
		return types.ActionContinue
	}
	stage := ctx.GetStringContext("gray-stage", "stable")
	if err := proxywasm.ReplaceHttpResponseHeader("x-gray-user", stage); err != nil {
		proxywasm.LogWarn("gray-whitelist: cannot write response routing header")
	}
	return types.ActionContinue
}

func headerFailure() types.Action {
	proxywasm.LogError("gray-whitelist: cannot replace routing header")
	_ = proxywasm.SendHttpResponse(503, nil, []byte("Gateway header processing failed"), -1)
	return types.ActionPause
}


func resolveStage(headers [][2]string, trustRequestHeader bool, cookieName, tenantClaim, userClaim string, contains func(string) bool) string {
	if trustRequestHeader {
		if stage, ok := findGrayHeader(headers); ok {
			return stage
		}
	}
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
	stage := resolveStage(headers, false, cookieName, tenantClaim, userClaim, contains)
	return rewriteHeadersWithStage(headers, stage)
}

func findGrayHeader(headers [][2]string) (string, bool) {
	stage := ""
	count := 0
	for _, h := range headers {
		if !strings.EqualFold(h[0], "x-gray-user") {
			continue
		}
		count++
		if h[1] == "canary" || h[1] == "stable" {
			stage = h[1]
		}
	}
	return stage, count == 1 && stage != ""
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
