package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm/types"
	hosttest "github.com/higress-group/wasm-go/pkg/test"
)

func TestGatewayPathMatching(t *testing.T) {
	cfg := settings{IncludePathPrefixes: []string{"/api/", "/tpi/"}}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/api/users", true}, {"/api/users?next=/index.html", true},
		{"/api/internal/v1/pc/login/actions/exchangeToken", true},
		{"/api/internal/v1/pc/logout", true}, {"/api/v1/cs/users", true},
		{"/api/partners/", true}, {"/api/v1/integrator", true}, {"/api/v1/integratorExtra", true},
		{"/tpi/users", true}, {"/api", false}, {"/API/users", false}, {"/index.html?next=/api/users", false},
	} {
		if got := gatewayPathEnabled([][2]string{{":path", tc.path}}, cfg); got != tc.want {
			t.Fatalf("%s: %t", tc.path, got)
		}
	}
	if gatewayPathEnabled(nil, cfg) || gatewayPathEnabled([][2]string{{":path", "/api/users"}}, settings{}) {
		t.Fatal("missing path/include must skip")
	}
}

func TestGatewayPathValidation(t *testing.T) {
	for _, key := range []string{"frontend_gray_cookie_excluded_paths", "gateway_exclude_path_prefixes"} {
		for _, value := range []any{nil, []string{}, []string{"/api/"}} {
			raw, _ := json.Marshal(map[string]any{"redis_cluster": "test", key: value})
			if _, err := decodeSettings(string(raw)); err == nil {
				t.Fatalf("removed configuration %s must be rejected", key)
			}
		}
	}
	for _, prefix := range []string{"", "api/", "/api/*", "/api/?x", "/api/#x", "/api/ "} {
		raw, _ := json.Marshal(map[string]any{"redis_cluster": "test", "gateway_include_path_prefixes": []string{prefix}})
		if _, err := decodeSettings(string(raw)); err == nil {
			t.Fatalf("accepted invalid prefix %q", prefix)
		}
	}
}

func TestGatewayRequestGateHooks(t *testing.T) {
	token := testJWT(`{"tenantId":2,"id":9}`)
	for _, mode := range []string{"whitelist", "traffic"} {
		t.Run(mode, func(t *testing.T) {
			for _, tc := range []struct {
				name, path, cookieName, cookie, state, extraCookie string
				emptyInclude, enabled                              bool
			}{
				{name: "included", path: "/api/users?x=1", cookie: "PC_AUTH_TOKEN=" + token, enabled: true},
				{name: "exchange_with_token", path: "/api/internal/v1/pc/login/actions/exchangeToken", cookie: "PC_AUTH_TOKEN=" + token, enabled: true},
				{name: "logout_with_token", path: "/api/internal/v1/pc/logout", cookie: "PC_AUTH_TOKEN=" + token, enabled: true},
				{name: "formerly_excluded", path: "/api/v1/cs/users", cookie: "PC_AUTH_TOKEN=" + token, enabled: true},
				{name: "custom_cookie", path: "/api/users", cookieName: "APP_TOKEN", cookie: "other=1; APP_TOKEN=" + token, enabled: true},
				{name: "missing_token", path: "/api/users"},
				{name: "empty_token", path: "/api/users", cookie: "PC_AUTH_TOKEN="},
				{name: "blank_token", path: "/api/users", cookie: "PC_AUTH_TOKEN= \t"},
				{name: "token_name_only", path: "/api/users", cookie: "PC_AUTH_TOKEN"},
				{name: "wrong_cookie_name", path: "/api/users", cookieName: "APP_TOKEN", cookie: "PC_AUTH_TOKEN=" + token},
				{name: "wrong_cookie_case", path: "/api/users", cookie: "pc_auth_token=" + token},
				{name: "duplicate_token", path: "/api/users", cookie: "PC_AUTH_TOKEN=" + token + "; PC_AUTH_TOKEN=" + token},
				{name: "duplicate_across_headers", path: "/api/users", cookie: "PC_AUTH_TOKEN=" + token, extraCookie: "PC_AUTH_TOKEN=" + token},
				{name: "static_with_token", path: "/index.html", cookie: "PC_AUTH_TOKEN=" + token},
				{name: "empty_include", path: "/api/users", cookie: "PC_AUTH_TOKEN=" + token, emptyInclude: true},
				{name: "disabled_with_token", path: "/api/users", cookie: "PC_AUTH_TOKEN=" + token, state: "disabled", enabled: true},
				{name: "cold_with_token", path: "/api/users", cookie: "PC_AUTH_TOKEN=" + token, state: "cold", enabled: true},
				{name: "expired_with_token", path: "/api/users", cookie: "PC_AUTH_TOKEN=" + token, state: "expired", enabled: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					hosttest.RunGoTest(t, func(t *testing.T) {
						include := []string{"/api/"}
						if tc.emptyInclude {
							include = nil
						}
						cookieName := tc.cookieName
						if cookieName == "" {
							cookieName = "PC_AUTH_TOKEN"
						}
						syncPath, _, _ := strings.Cut(tc.path, "?")
						raw, _ := json.Marshal(map[string]any{"redis_cluster": "test", "gateway_include_path_prefixes": include, "jwt_token_cookie_name": cookieName, "frontend_gray_cookie_name": "x-gray-frontend", "gateway_frontend_gray_cookie_include_path_prefixes": []string{syncPath}})
						host, status := hosttest.NewTestHost(raw)
						defer host.Reset()
						if status != types.OnPluginStartStatusOK {
							t.Fatal("start failed")
						}
						config, err := host.GetMatchConfig()
						if err != nil {
							t.Fatal(err)
						}
						r := config.(*Config).runtime
						now := time.Now()
						r.now = func() time.Time {
							if !tc.enabled {
								t.Fatal("skipped request reached gray judgment")
							}
							return now
						}
						r.snapshot = &graySnapshot{enabled: true, mode: mode, percent: 100, loadedAt: now, users: map[string]struct{}{"user:9": {}}}
						wantStage := "canary"
						switch tc.state {
						case "disabled":
							r.snapshot.enabled = false
							wantStage = "stable"
						case "cold":
							r.snapshot = nil
							wantStage = "stable"
						case "expired":
							r.snapshot.loadedAt = now.Add(-time.Duration(r.cfg.TTL) * time.Second)
							wantStage = "stable"
						}
						cookie := tc.cookie
						if !tc.enabled || wantStage == "stable" {
							// Skipped requests must preserve an existing gray Cookie even on 2xx.
							cookie += "; x-gray-frontend=always"
						}
						request := [][2]string{{":method", "POST"}, {":path", tc.path}, {":authority", "example.com"}, {"cookie", cookie}, {"x-gray-user", "original"}}
						if tc.extraCookie != "" {
							request = append(request, [2]string{"cookie", tc.extraCookie})
						}
						if host.CallOnHttpRequestHeaders(request) != types.ActionContinue {
							t.Fatal("blocked")
						}
						wantRequest := request
						if tc.enabled {
							wantRequest = rewriteHeadersWithStage(request, wantStage)
						}
						if !reflect.DeepEqual(wantRequest, host.GetRequestHeaders()) {
							t.Fatalf("request headers: %v", host.GetRequestHeaders())
						}
						// Response handling must use the saved request decision.
						r.cfg.IncludePathPrefixes = nil
						r.cfg.TokenCookieName = "CHANGED_TOKEN"
						response := [][2]string{{":status", "200"}, {"set-cookie", "PC_AUTH_TOKEN=new; HttpOnly"}, {"x-gray-user", "upstream"}}
						if !tc.enabled {
							response = append(response, [2]string{"x-frontend-gray-refresh", "upstream"})
						}
						host.CallOnHttpResponseHeaders(response)
						if !tc.enabled {
							if !reflect.DeepEqual(response, host.GetResponseHeaders()) {
								t.Fatal("skipped response mutated")
							}
						} else {
							foundStage, foundRefresh, foundCookie, foundLogin := false, false, false, false
							for _, h := range host.GetResponseHeaders() {
								if h[0] == "x-gray-user" && h[1] == wantStage {
									foundStage = true
								}
								if h[0] == "x-frontend-gray-refresh" && h[1] == "true" {
									foundRefresh = true
								}
								if strings.EqualFold(h[0], "set-cookie") {
									// The SDK mock concatenates repeated response header values.
									foundLogin = foundLogin || strings.Contains(h[1], "PC_AUTH_TOKEN=new; HttpOnly")
									foundCookie = foundCookie || strings.Contains(h[1], buildFrontendGrayCookie(r.cfg, wantStage))
								}
							}
							if !foundStage || !foundRefresh || !foundCookie || !foundLogin {
								t.Fatalf("response behavior: %v", host.GetResponseHeaders())
							}
						}
						if len(host.GetRedisCalloutAttributes()) != 0 {
							t.Fatal("request issued Redis call")
						}
					})
				})
			}
		})
	}
}
