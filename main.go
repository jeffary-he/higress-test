package main

import (
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
	ConnectivityTestEnabled bool   `json:"connectivity_test_enabled"`
	ConnectivityTestKey     string `json:"connectivity_test_key"`
	ConnectivityTestPeriod  int64  `json:"connectivity_test_interval_ms"`
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
	wrapper.SetCtx("gray-whitelist", wrapper.ParseConfig(parseConfig), wrapper.ProcessRequestHeaders(onRequestHeaders))
}

func decodeSettings(raw string) (settings, error) {
	cfg := settings{
		Database: 0, Key: "gray:whitelist:token-sha256:v1", Timeout: 1000,
		Refresh: 10000, TTL: 60000, MaxEntries: 10000,
		ConnectivityTestKey: "gray:whitelist:connectivity-test:v1", ConnectivityTestPeriod: 60000,
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
		if member.Type() != resp.BulkString || member.IsNull() || !whitelist.ValidDigest(member.String()) {
			return nil, errors.New("invalid digest")
		}
		next[member.String()] = struct{}{}
	}
	return next, nil
}

func onRequestHeaders(_ wrapper.HttpContext, cfg Config) types.Action {
	headers, err := proxywasm.GetHttpRequestHeaders()
	if err != nil {
		return headerFailure()
	}
	output := rewriteHeaders(headers, func(digest string) bool {
		return cfg.runtime != nil && cfg.runtime.cache.Contains(digest, cfg.runtime.now())
	})
	// Remove all client gray headers. Keep Authorization untouched; allow rerouting.
	if err := proxywasm.ReplaceHttpRequestHeaders(output); err != nil {
		return headerFailure()
	}
	return types.ActionContinue
}

func headerFailure() types.Action {
	proxywasm.LogError("gray-whitelist: cannot replace routing header")
	_ = proxywasm.SendHttpResponse(503, nil, []byte("Gateway header processing failed"), -1)
	return types.ActionPause
}

func rewriteHeaders(headers [][2]string, contains func(string) bool) [][2]string {
	var auth string
	count := 0
	output := make([][2]string, 0, len(headers)+1)
	for _, h := range headers {
		if strings.EqualFold(h[0], "authorization") {
			auth = h[1]
			count++
		}
		if !strings.EqualFold(h[0], "x-gray-user") {
			output = append(output, h)
		}
	}
	stage := "stable"
	if count == 1 {
		if digest, ok := whitelist.Digest(auth); ok && contains(digest) {
			stage = "canary"
		}
	}
	return append(output, [2]string{"x-gray-user", stage})
}
