package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	hosttest "github.com/higress-group/wasm-go/pkg/test"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/resp"
)

type queuedRedis struct {
	wrapper.RedisClient
	commands    []string
	callbacks   []wrapper.RedisResponseCallback
	dispatchErr error
}

func (q *queuedRedis) Get(key string, cb wrapper.RedisResponseCallback) error {
	q.commands = append(q.commands, "GET "+key)
	q.callbacks = append(q.callbacks, cb)
	return q.dispatchErr
}
func (q *queuedRedis) SMembers(key string, cb wrapper.RedisResponseCallback) error {
	q.commands = append(q.commands, "SMEMBERS "+key)
	q.callbacks = append(q.callbacks, cb)
	return q.dispatchErr
}

func TestModeRefreshAndFailure(t *testing.T) {
	hosttest.RunGoTest(t, func(t *testing.T) {
		host, _ := hosttest.NewTestHost(json.RawMessage(`{"gateway_include_path_prefixes":["/"],"redis_cluster":"test"}`))
		defer host.Reset()
		cfg, _ := decodeSettings(`{"redis_cluster":"test"}`)
		now := time.Unix(1000, 0)
		q := &queuedRedis{}
		r := &runtimeState{cfg: cfg, client: q, now: func() time.Time { return now }}
		headers := [][2]string{{"cookie", "PC_AUTH_TOKEN=opaque-token"}}
		if r.stage(headers) != "stable" {
			t.Fatal("cold start")
		}
		r.refresh()
		q.callbacks[0](resp.StringValue("0"))
		if len(q.commands) != 1 || r.snapshot.enabled {
			t.Fatal("disabled must only read switch")
		}
		old := r.snapshot
		r.refresh()
		q.callbacks[1](resp.StringValue("1"))
		q.callbacks[2](resp.StringValue("traffic"))
		if r.snapshot != old || q.commands[3] != "GET "+cfg.PercentKey {
			t.Fatal("premature publish or wrong branch")
		}
		q.callbacks[3](resp.StringValue("100"))
		if r.stage(headers) != "canary" || len(q.commands) != 4 {
			t.Fatal("opaque token must work without JWT or sets")
		}
		old = r.snapshot
		now = now.Add(time.Second)
		r.refresh()
		q.callbacks[4](resp.StringValue("1"))
		q.callbacks[5](resp.StringValue("whitelist"))
		q.callbacks[6](resp.ArrayValue([]resp.Value{resp.StringValue("2")}))
		if r.snapshot != old {
			t.Fatal("partial whitelist replaced traffic")
		}
		q.callbacks[7](resp.ErrorValue(errors.New("offline")))
		if r.snapshot != old {
			t.Fatal("failure replaced snapshot")
		}
		now = old.loadedAt.Add(time.Minute)
		if r.stage(headers) != "stable" {
			t.Fatal("failure renewed TTL")
		}
		r.refresh()
		q.callbacks[8](resp.StringValue("1"))
		q.callbacks[9](resp.NullValue())
		q.callbacks[10](resp.ArrayValue([]resp.Value{}))
		q.callbacks[11](resp.ArrayValue([]resp.Value{resp.StringValue("9")}))
		if r.snapshot.mode != "whitelist" || r.snapshot.percent != 0 {
			t.Fatal("legacy default or stale traffic data")
		}
		jwt := testJWT(`{"id":9}`)
		if r.stage([][2]string{{"cookie", "PC_AUTH_TOKEN=" + jwt}}) != "canary" {
			t.Fatal("user OR match")
		}
		r.refresh()
		q.callbacks[12](resp.StringValue("0"))
		if r.snapshot.enabled || len(q.commands) != 13 {
			t.Fatal("shutdown must publish immediately")
		}
	})
}

func TestRefreshLateCallbackAndDispatchFailure(t *testing.T) {
	hosttest.RunGoTest(t, func(t *testing.T) {
		host, _ := hosttest.NewTestHost(json.RawMessage(`{"gateway_include_path_prefixes":["/"],"redis_cluster":"test"}`))
		defer host.Reset()
		cfg, _ := decodeSettings(`{"redis_cluster":"test"}`)
		now := time.Unix(1000, 0)
		q := &queuedRedis{}
		r := &runtimeState{cfg: cfg, client: q, now: func() time.Time { return now }}
		r.refresh()
		r.refresh()
		if len(q.commands) != 1 {
			t.Fatal("overlapping refresh")
		}
		now = now.Add(5 * time.Second)
		r.refresh()
		q.callbacks[1](resp.StringValue("0"))
		old := r.snapshot
		q.callbacks[0](resp.StringValue("1"))
		if len(q.commands) != 2 || r.snapshot != old {
			t.Fatal("stale callback continued refresh")
		}
		q.dispatchErr = errors.New("dispatch failed")
		r.refresh()
		if r.refreshing || r.snapshot != old {
			t.Fatal("dispatch failure")
		}
		q.dispatchErr = nil
		r.refresh()
		q.callbacks[3](resp.StringValue("1"))
		q.callbacks[4](resp.StringValue("invalid"))
		if r.refreshing || r.snapshot != old {
			t.Fatal("invalid mode")
		}
	})
}

func TestTrafficHashAndInput(t *testing.T) {
	// SHA-256("abc") starts ba7816bf8f01cfea: uint64 BE mod 100 = 74.
	if trafficStage("abc", 74) != "stable" || trafficStage("abc", 75) != "canary" {
		t.Fatal("hash or boundary differs from contract")
	}
	for _, percent := range []int{-1, 0, 101} {
		if trafficStage("abc", percent) != "stable" {
			t.Fatal("invalid/zero percentage")
		}
	}
	if trafficStage("", 100) != "stable" || trafficStage(strings.Repeat("x", 16385), 100) != "stable" {
		t.Fatal("token bounds")
	}
	cfg, _ := decodeSettings(`{"redis_cluster":"test"}`)
	now := time.Now()
	r := &runtimeState{cfg: cfg, now: func() time.Time { return now }, snapshot: &graySnapshot{enabled: true, mode: "traffic", percent: 100, loadedAt: now}}
	for _, cookie := range []string{"", "PC_AUTH_TOKEN=", "PC_AUTH_TOKEN=a; PC_AUTH_TOKEN=b"} {
		if r.stage([][2]string{{"cookie", cookie}}) != "stable" {
			t.Fatal("missing/duplicate token")
		}
	}
	for _, text := range []string{"-1", "101", "10.5", "true", "", " 10", "+10", "01"} {
		if _, ok := parsePercent(resp.StringValue(text)); ok {
			t.Fatalf("accepted %q", text)
		}
	}
	for _, text := range []string{"0", "10", "100"} {
		if _, ok := parsePercent(resp.StringValue(text)); !ok {
			t.Fatalf("rejected %q", text)
		}
	}
}

func TestTrafficResponseCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, status, cookie string
		percent              int
		stage, setCookie     string
		refresh              bool
	}{
		{"enter", "200", "", 100, "canary", "x-gray-frontend=always;", true},
		{"stay", "200", "; x-gray-frontend=always", 100, "canary", "", false},
		{"leave", "200", "; x-gray-frontend=always", 0, "stable", "Max-Age=0", true},
		{"stable", "200", "", 0, "stable", "", false},
		{"redirect", "302", "", 100, "", "", false},
		{"unauthorized", "401", "", 100, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hosttest.RunGoTest(t, func(t *testing.T) {
				host, _ := hosttest.NewTestHost(json.RawMessage(`{"gateway_include_path_prefixes":["/"],"redis_cluster":"test","frontend_gray_cookie_name":"x-gray-frontend","gateway_frontend_gray_cookie_include_path_prefixes":["/api/status"]}`))
				defer host.Reset()
				config, err := host.GetMatchConfig()
				if err != nil {
					t.Fatal(err)
				}
				r := config.(*Config).runtime
				r.snapshot = &graySnapshot{enabled: true, mode: "traffic", percent: tc.percent, loadedAt: r.now()}
				host.CallOnHttpRequestHeaders([][2]string{
					{":method", "GET"}, {":path", "/api/status"}, {":authority", "example.com"},
					{"cookie", "PC_AUTH_TOKEN=opaque-token" + tc.cookie}, {"x-gray-user", "forged"},
				})
				want := "stable"
				if tc.percent == 100 {
					want = "canary"
				}
				found := 0
				for _, h := range host.GetRequestHeaders() {
					if h[0] == "x-gray-user" {
						found++
						if h[1] != want {
							t.Fatal("request stage")
						}
					}
				}
				if found != 1 {
					t.Fatal("request header duplicates")
				}
				host.CallOnHttpResponseHeaders([][2]string{{":status", tc.status}})
				result := map[string]string{}
				for _, h := range host.GetResponseHeaders() {
					result[strings.ToLower(h[0])] = h[1]
				}
				if result["x-gray-user"] != tc.stage {
					t.Fatal("response stage")
				}
				if tc.setCookie == "" {
					if result["set-cookie"] != "" {
						t.Fatal("unexpected cookie")
					}
				} else if !strings.Contains(result["set-cookie"], tc.setCookie) {
					t.Fatal("cookie missing")
				}
				if (result["x-frontend-gray-refresh"] == "true") != tc.refresh {
					t.Fatal("refresh header")
				}
				if len(host.GetRedisCalloutAttributes()) != 0 {
					t.Fatal("request path queried Redis")
				}
			})
		})
	}
}

func TestInvalidPercentRetainsWholeSnapshot(t *testing.T) {
	hosttest.RunGoTest(t, func(t *testing.T) {
		host, _ := hosttest.NewTestHost(json.RawMessage(`{"gateway_include_path_prefixes":["/"],"redis_cluster":"test"}`))
		defer host.Reset()
		cfg, _ := decodeSettings(`{"redis_cluster":"test"}`)
		now := time.Now()
		old := &graySnapshot{enabled: true, mode: "whitelist", loadedAt: now, tenants: map[string]struct{}{"tenant:2": {}}}
		q := &queuedRedis{}
		r := &runtimeState{cfg: cfg, client: q, now: func() time.Time { return now }, snapshot: old}
		r.refresh()
		q.callbacks[0](resp.StringValue("1"))
		q.callbacks[1](resp.StringValue("traffic"))
		q.callbacks[2](resp.NullValue())
		if r.snapshot != old || r.refreshing {
			t.Fatal("invalid ratio changed active config")
		}
		if len(q.commands) != 3 {
			t.Fatal("traffic read whitelist")
		}
	})
}
