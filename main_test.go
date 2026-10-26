package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	hosttest "github.com/higress-group/wasm-go/pkg/test"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/resp"
)

type fakeRedis struct {
	wrapper.RedisClient
	callbacks     []wrapper.RedisResponseCallback
	incrCallbacks []wrapper.RedisResponseCallback
	incrKeys      []string
}

func (f *fakeRedis) SMembers(_ string, cb wrapper.RedisResponseCallback) error {
	f.callbacks = append(f.callbacks, cb)
	return nil
}

func (f *fakeRedis) Get(key string, cb wrapper.RedisResponseCallback) error {
	if key == "mode" {
		cb(resp.StringValue("whitelist"))
	} else {
		cb(resp.StringValue("1"))
	}
	return nil
}

func (f *fakeRedis) Incr(key string, cb wrapper.RedisResponseCallback) error {
	f.incrKeys = append(f.incrKeys, key)
	f.incrCallbacks = append(f.incrCallbacks, cb)
	return nil
}

func TestConnectivityWriteProbe(t *testing.T) {
	hosttest.RunGoTest(t, func(t *testing.T) {
		host, _ := hosttest.NewTestHost(json.RawMessage(`{"redis_cluster":"test"}`))
		defer host.Reset()
		client := &fakeRedis{}
		r := &runtimeState{
			cfg:    settings{ConnectivityTestKey: "gray:test"},
			client: client,
		}
		r.writeConnectivityProbe()
		if len(client.incrKeys) != 1 || client.incrKeys[0] != "gray:test" {
			t.Fatal("connectivity probe did not increment configured key")
		}
		client.incrCallbacks[0](resp.IntegerValue(7))
	})
}

func TestRefreshCallbacks(t *testing.T) {
	hosttest.RunGoTest(t, func(t *testing.T) {
		host, _ := hosttest.NewTestHost(json.RawMessage(`{"redis_cluster":"test"}`))
		defer host.Reset()
		now := time.Now()
		client := &fakeRedis{}
		r := &runtimeState{
			cfg:    settings{ModeKey: "mode", TenantKey: "tenant", UserKey: "user", Timeout: 1000, MaxEntries: 10},
			client: client, now: func() time.Time { return now },
		}
		selector := "tenant:2"
		snapshot := resp.ArrayValue([]resp.Value{resp.StringValue("2")})
		r.refresh()
		if len(client.callbacks) != 1 {
			t.Fatal("tenant refresh should precede user refresh")
		}
		client.callbacks[0](snapshot)
		if r.snapshot != nil {
			t.Fatal("partial snapshot published")
		}
		client.callbacks[1](resp.ArrayValue([]resp.Value{}))
		if _, ok := r.snapshot.tenants[selector]; !ok {
			t.Fatal("snapshot not loaded")
		}
		now = now.Add(10 * time.Second)
		r.refresh()
		client.callbacks[2](resp.ErrorValue(errors.New("offline")))
		if len(client.callbacks) != 3 {
			t.Fatal("failed refresh should stop reading")
		}
		if _, ok := r.snapshot.tenants[selector]; !ok {
			t.Fatal("failure discarded cache")
		}
	})
}

func TestSettingsValidation(t *testing.T) {
	cfg, err := decodeSettings(`{"redis_cluster":"outbound|6379||aws-redis.dns","redis_database":15}`)
	if err != nil || cfg.Database != 15 || cfg.TTL != 60000 || cfg.Refresh != 10000 || cfg.EnabledKey != "gray:whitelist:pc:enabled" ||
		cfg.TokenCookieName != "PC_AUTH_TOKEN" || cfg.TenantIDClaim != "tenantId" || cfg.UserIDClaim != "id" ||
		cfg.ConnectivityTestEnabled || cfg.ConnectivityTestPeriod != 60000 || !cfg.ResponseHeaderEnabled {
		t.Fatal("default config invalid")
	}
	for _, bad := range []string{
		`{}`, `{"redis_cluster":"x","redis_timeout_ms":-1}`,
		`{"redis_cluster":"x","redis_database":16}`, `{"redis_cluster":"x","redis_database":1.5}`,
		`{"redis_cluster":"x","cache_ttl_ms":1000}`, `{"redis_cluster":"x","refresh_interval_ms":1001}`,
		`{"redis_cluster":"x","max_entries":0}`,
		`{"redis_cluster":"x","token_cookie_name":"bad name"}`,
		`{"redis_cluster":"x","tenant_id_claim":"bad.name"}`,
		`{"redis_cluster":"x","tenant_id_claim":"id","user_id_claim":"id"}`,
		`{"redis_cluster":"x","connectivity_test_enabled":true,"connectivity_test_key":""}`,
		`{"redis_cluster":"x","connectivity_test_enabled":true,"connectivity_test_interval_ms":1001}`,
	} {
		if _, err := decodeSettings(bad); err == nil {
			t.Errorf("accepted invalid config: %s", bad)
		}
	}
}

func TestSnapshotValidation(t *testing.T) {
	selector := "tenant:2"
	valid := resp.ArrayValue([]resp.Value{resp.StringValue(selector)})
	if next, err := parseSnapshot(valid, 1); err != nil || len(next) != 1 {
		t.Fatal("valid snapshot rejected")
	}
	if next, err := parseSnapshot(resp.ArrayValue([]resp.Value{}), 1); err != nil || len(next) != 0 {
		t.Fatal("empty snapshot rejected")
	}
	for _, v := range []resp.Value{
		resp.ErrorValue(errors.New("unavailable")), resp.StringValue("OK"), resp.IntegerValue(1),
		resp.ArrayValue([]resp.Value{resp.StringValue("plain-user")}),
		resp.ArrayValue([]resp.Value{resp.StringValue("tenant:02")}),
		resp.ArrayValue([]resp.Value{resp.IntegerValue(1)}),
		resp.ArrayValue([]resp.Value{resp.StringValue(selector), resp.StringValue(selector)}),
	} {
		if _, err := parseSnapshot(v, 1); err == nil {
			t.Fatal("invalid/oversized snapshot accepted")
		}
	}
}

func TestHeaderMatching(t *testing.T) {
	canaryToken := testJWT(`{"tenantId":2,"id":283778812672}`)
	stableToken := testJWT(`{"tenantId":3,"id":99}`)
	for _, tc := range []struct {
		headers [][2]string
		allowed string
		stage   string
	}{
		{nil, "tenant:2", "stable"},
		{[][2]string{{"Cookie", "other=1"}}, "tenant:2", "stable"},
		{[][2]string{{"Cookie", "_ga=1; PC_AUTH_TOKEN=" + canaryToken + "; other=2"}}, "tenant:2", "canary"},
		{[][2]string{{"Cookie", "PC_AUTH_TOKEN=" + canaryToken}}, "user:283778812672", "canary"},
		{[][2]string{{"cookie", "PC_AUTH_TOKEN=" + stableToken}}, "tenant:2", "stable"},
		{[][2]string{{"Cookie", "PC_AUTH_TOKEN=bad.jwt"}}, "tenant:2", "stable"},
		{[][2]string{{"Cookie", "PC_AUTH_TOKEN=" + canaryToken + "; PC_AUTH_TOKEN=" + stableToken}}, "tenant:2", "stable"},
	} {
		in := append([][2]string{{"X-Gray-User", "canary"}, {"x-gray-user", "canary"}}, tc.headers...)
		out := rewriteHeaders(in, "PC_AUTH_TOKEN", "tenantId", "id", func(v string) bool { return v == tc.allowed })
		if len(out) != len(tc.headers)+1 || out[len(out)-1] != [2]string{"x-gray-user", tc.stage} {
			t.Fatal("incorrect routing label/duplicate removal")
		}
		for i, h := range tc.headers {
			if out[i] != h {
				t.Fatal("Cookie changed")
			}
		}
	}
}

func TestJWTSelectorDecoding(t *testing.T) {
	for _, payload := range []string{
		`{"tenantId":2,"id":283778812672}`,
		`{"tenantId":"2","id":"283778812672"}`,
	} {
		got := decodeJWTSelectors(testJWT(payload), "tenantId", "id")
		if len(got) != 2 || got[0] != "tenant:2" || got[1] != "user:283778812672" {
			t.Fatal("valid claims rejected")
		}
	}
	if got := decodeJWTSelectors(testJWT(`{"tenantId":2}`), "tenantId", "id"); len(got) != 1 || got[0] != "tenant:2" {
		t.Fatal("tenant-only selector rejected")
	}
	if got := decodeJWTSelectors(testJWT(`{"id":9}`), "tenantId", "id"); len(got) != 1 || got[0] != "user:9" {
		t.Fatal("user-only selector rejected")
	}
	for _, payload := range []string{`{}`, `{"tenantId":"02"}`, `{"tenantId":2.5}`, `{"id":true}`} {
		if got := decodeJWTSelectors(testJWT(payload), "tenantId", "id"); len(got) != 0 {
			t.Fatal("invalid claims accepted")
		}
	}
	if got := decodeJWTSelectors("bad.jwt", "tenantId", "id"); len(got) != 0 {
		t.Fatal("invalid JWT accepted")
	}
}

func testJWT(payload string) string {
	encode := base64.RawURLEncoding.EncodeToString
	return encode([]byte(`{"alg":"none"}`)) + "." + encode([]byte(payload)) + ".unsigned"
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
		selector := "tenant:2"
		token := testJWT(`{"tenantId":2,"id":283778812672}`)
		r.snapshot = &graySnapshot{enabled: true, mode: "whitelist", loadedAt: now, tenants: map[string]struct{}{selector: {}}}
		action := host.CallOnHttpRequestHeaders([][2]string{
			{":method", "GET"}, {":path", "/"}, {":authority", "example.com"},
			{"cookie", "_ga=1; PC_AUTH_TOKEN=" + token},
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

func TestCookieExcludedPath(t *testing.T) {
	const path = "/api/internal/v1/pc/login/actions/exchangeToken"
	for _, tc := range []struct {
		path string
		want bool
	}{
		{path, true}, {path + "?code=123", true}, {path + "/extra", false}, {"/api/other", false},
	} {
		if got := frontendCookieExcluded([][2]string{{":path", tc.path}}, []string{path}); got != tc.want {
			t.Fatalf("path %s: got %t", tc.path, got)
		}
	}
}

func TestExcludedCookieHooks(t *testing.T) {
	for _, canary := range []bool{false, true} {
		hosttest.RunGoTest(t, func(t *testing.T) {
			host, status := hosttest.NewTestHost(json.RawMessage(`{"redis_cluster":"redis","frontend_gray_cookie_name":"x-gray-frontend","frontend_gray_cookie_excluded_paths":["/api/internal/v1/pc/login/actions/exchangeToken"]}`))
			defer host.Reset()
			if status != types.OnPluginStartStatusOK {
				t.Fatal("configuration failed")
			}
			cfg, err := host.GetMatchConfig()
			if err != nil {
				t.Fatal(err)
			}
			r := cfg.(*Config).runtime
			r.snapshot = &graySnapshot{enabled: true, mode: "whitelist", loadedAt: time.Now(), tenants: map[string]struct{}{"tenant:2": {}}}
			cookie, stage := "x-gray-frontend=always", "stable"
			if canary {
				cookie = "PC_AUTH_TOKEN=" + testJWT(`{"tenantId":2}`)
				stage = "canary"
			}
			host.CallOnHttpRequestHeaders([][2]string{{":method", "POST"}, {":path", "/api/internal/v1/pc/login/actions/exchangeToken?code=123"}, {":authority", "example.com"}, {"cookie", cookie}})
			found := false
			for _, h := range host.GetRequestHeaders() {
				if h[0] == "x-gray-user" {
					found = h[1] == stage
				}
			}
			if !found {
				t.Fatal("routing changed")
			}
			host.CallOnHttpResponseHeaders([][2]string{{":status", "200"}, {"set-cookie", "PC_AUTH_TOKEN=new; HttpOnly"}})
			cookies := 0
			for _, h := range host.GetResponseHeaders() {
				if h[0] == "x-frontend-gray-refresh" {
					t.Fatal("unexpected refresh")
				}
				if h[0] == "set-cookie" || h[0] == "Set-Cookie" {
					cookies++
					if h[1] != "PC_AUTH_TOKEN=new; HttpOnly" {
						t.Fatal("unexpected cookie mutation")
					}
				}
			}
			if cookies != 1 {
				t.Fatal("login cookie lost")
			}
		})
	}
}

func TestRequestHeaderTakesPrecedence(t *testing.T) {
	headers := [][2]string{{"x-gray-user", "canary"}, {"cookie", "PC_AUTH_TOKEN=invalid"}}
	got := resolveStage(headers, "PC_AUTH_TOKEN", "tenantId", "id", func(string) bool { return false })
	if got != "stable" {
		t.Fatalf("client routing header should not be trusted: %s", got)
	}
}
