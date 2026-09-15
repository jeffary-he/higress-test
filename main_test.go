package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	hosttest "github.com/higress-group/wasm-go/pkg/test"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/resp"
	"gray-whitelist-wasm/internal/whitelist"
)

type fakeRedis struct {
	wrapper.RedisClient
	callbacks []wrapper.RedisResponseCallback
}

func (f *fakeRedis) SMembers(_ string, cb wrapper.RedisResponseCallback) error {
	f.callbacks = append(f.callbacks, cb)
	return nil
}

func TestRefreshCallbacks(t *testing.T) {
	hosttest.RunGoTest(t, func(t *testing.T) {
		host, _ := hosttest.NewTestHost(json.RawMessage(`{"redis_cluster":"test"}`))
		defer host.Reset()
		now := time.Now()
		client := &fakeRedis{}
		r := &runtimeState{
			cfg:    settings{Key: "test", Timeout: 1000, MaxEntries: 10},
			client: client, now: func() time.Time { return now },
			cache: whitelist.NewCache(time.Minute),
		}
		digest, _ := whitelist.Digest("abc")
		snapshot := resp.ArrayValue([]resp.Value{resp.StringValue(digest)})
		r.refresh()
		r.refresh()
		if len(client.callbacks) != 1 {
			t.Fatal("overlapping refresh")
		}
		client.callbacks[0](snapshot)
		if !r.cache.Contains(digest, now) {
			t.Fatal("snapshot not loaded")
		}
		now = now.Add(10 * time.Second)
		r.refresh()
		client.callbacks[1](resp.ErrorValue(errors.New("offline")))
		if !r.cache.Contains(digest, now) {
			t.Fatal("failure discarded cache")
		}
		now = now.Add(10 * time.Second)
		r.refresh()
		now = now.Add(2 * time.Second)
		r.refresh()
		client.callbacks[3](resp.ArrayValue([]resp.Value{}))
		client.callbacks[2](snapshot)
		if r.cache.Contains(digest, now) {
			t.Fatal("late response replaced empty snapshot")
		}
	})
}

func TestSettingsValidation(t *testing.T) {
	cfg, err := decodeSettings(`{"redis_cluster":"outbound|6379||aws-redis.dns","redis_database":15}`)
	if err != nil || cfg.Database != 15 || cfg.TTL != 60000 || cfg.Refresh != 10000 {
		t.Fatal("default config invalid")
	}
	for _, bad := range []string{
		`{}`, `{"redis_cluster":"x","redis_timeout_ms":-1}`,
		`{"redis_cluster":"x","redis_database":16}`, `{"redis_cluster":"x","redis_database":1.5}`,
		`{"redis_cluster":"x","cache_ttl_ms":1000}`, `{"redis_cluster":"x","refresh_interval_ms":1001}`,
		`{"redis_cluster":"x","max_entries":0}`,
	} {
		if _, err := decodeSettings(bad); err == nil {
			t.Errorf("accepted invalid config: %s", bad)
		}
	}
}

func TestSnapshotValidation(t *testing.T) {
	digest, _ := whitelist.Digest("abc")
	valid := resp.ArrayValue([]resp.Value{resp.StringValue(digest)})
	if next, err := parseSnapshot(valid, 1); err != nil || len(next) != 1 {
		t.Fatal("valid snapshot rejected")
	}
	if next, err := parseSnapshot(resp.ArrayValue([]resp.Value{}), 1); err != nil || len(next) != 0 {
		t.Fatal("empty snapshot rejected")
	}
	for _, v := range []resp.Value{
		resp.ErrorValue(errors.New("unavailable")), resp.StringValue("OK"), resp.IntegerValue(1),
		resp.ArrayValue([]resp.Value{resp.StringValue("plain-token")}),
		resp.ArrayValue([]resp.Value{resp.StringValue(strings.ToUpper(digest))}),
		resp.ArrayValue([]resp.Value{resp.IntegerValue(1)}),
		resp.ArrayValue([]resp.Value{resp.StringValue(digest), resp.StringValue(digest)}),
	} {
		if _, err := parseSnapshot(v, 1); err == nil {
			t.Fatal("invalid/oversized snapshot accepted")
		}
	}
}

func TestHeaderMatching(t *testing.T) {
	digest, _ := whitelist.Digest("AbC123")
	for _, tc := range []struct {
		auth  [][2]string
		stage string
	}{
		{nil, "stable"},
		{[][2]string{{"Authorization", "Bearer "}}, "stable"},
		{[][2]string{{"Authorization", "Bearer AbC123"}}, "canary"},
		{[][2]string{{"authorization", "AbC123"}}, "canary"},
		{[][2]string{{"Authorization", "Bearer abc123"}}, "stable"},
		{[][2]string{{"Authorization", "Bearer AbC123"}, {"authorization", "bad"}}, "stable"},
	} {
		in := append([][2]string{{"X-Gray-User", "canary"}, {"x-gray-user", "canary"}}, tc.auth...)
		out := rewriteHeaders(in, func(d string) bool { return d == digest })
		if len(out) != len(tc.auth)+1 || out[len(out)-1] != [2]string{"x-gray-user", tc.stage} {
			t.Fatal("incorrect routing label/duplicate removal")
		}
		for i, h := range tc.auth {
			if out[i] != h {
				t.Fatal("Authorization changed")
			}
		}
	}
}

func TestGatewayHeaderHook(t *testing.T) {
	hosttest.RunGoTest(t, func(t *testing.T) {
		host, status := hosttest.NewTestHost(json.RawMessage(`{"redis_cluster":"outbound|6379||aws-redis.dns"}`))
		defer host.Reset()
		if status != types.OnPluginStartStatusOK {
			t.Fatal("plugin failed to configure")
		}
		cfg, err := host.GetMatchConfig()
		if err != nil {
			t.Fatal(err)
		}
		r := cfg.(*Config).runtime
		now := time.Now()
		digest, _ := whitelist.Digest("AbC123")
		id, _ := r.cache.Begin(now, time.Second)
		r.cache.Finish(id, now, now, map[string]struct{}{digest: {}}, true)
		action := host.CallOnHttpRequestHeaders([][2]string{
			{":method", "GET"}, {":path", "/"}, {":authority", "example.com"},
			{"authorization", "Bearer AbC123"}, {"x-gray-user", "stable"},
		})
		if action != types.ActionContinue {
			t.Fatal("request unexpectedly blocked")
		}
		found := 0
		for _, h := range host.GetRequestHeaders() {
			if h[0] == "x-gray-user" {
				found++
				if h[1] != "canary" {
					t.Fatal("wrong stage")
				}
			}
		}
		if found != 1 {
			t.Fatal("missing/duplicate routing header")
		}
		// Request-path processing must not dispatch Redis calls.
		if len(host.GetRedisCalloutAttributes()) != 0 {
			t.Fatal("request queried Redis")
		}
	})
}
