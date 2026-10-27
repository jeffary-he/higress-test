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
	cfg := settings{IncludePathPrefixes: []string{"/api/"}, ExcludePathPrefixes: []string{"/api/internal/v1/pc/login/actions/exchangeToken", "/api/v1/cs/", "/api/partners/", "/api/v1/integrator"}}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/api/users", true}, {"/api/users?next=/api/v1/cs/", true},
		{"/api/internal/v1/pc/login/actions/exchangeToken", false},
		{"/api/internal/v1/pc/login/actions/exchangeTokenOther?x=1", false},
		{"/api/v1/cs/users", false}, {"/api/partners/", false},
		{"/api/v1/integrator", false}, {"/api/v1/integratorExtra", false},
		{"/api/v1/cs", true}, {"/api", false}, {"/API/users", false}, {"/index.html", false},
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
	for _, key := range []string{"gateway_include_path_prefixes", "gateway_exclude_path_prefixes"} {
		for _, prefix := range []string{"", "api/", "/api/*", "/api/?x", "/api/#x", "/api/ "} {
			raw, _ := json.Marshal(map[string]any{"redis_cluster": "test", key: []string{prefix}})
			if _, err := decodeSettings(string(raw)); err == nil {
				t.Fatalf("accepted %s=%q", key, prefix)
			}
		}
	}
}

func TestGatewayPathHooks(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		include    []string
		enabled    bool
	}{
		{"included", "/api/users?x=1", []string{"/api/"}, true},
		{"excluded", "/api/v1/cs/users", []string{"/api/"}, false},
		{"static", "/index.html", []string{"/api/"}, false},
		{"empty", "/api/users", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hosttest.RunGoTest(t, func(t *testing.T) {
				raw, _ := json.Marshal(map[string]any{"redis_cluster": "test", "gateway_include_path_prefixes": tc.include, "gateway_exclude_path_prefixes": []string{"/api/v1/cs/"}, "frontend_gray_cookie_name": "x-gray-frontend"})
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
				r.snapshot = &graySnapshot{enabled: true, mode: "traffic", percent: 100, loadedAt: time.Now()}
				request := [][2]string{{":method", "GET"}, {":path", tc.path}, {":authority", "example.com"}, {"cookie", "PC_AUTH_TOKEN=opaque"}, {"x-gray-user", "original"}}
				if host.CallOnHttpRequestHeaders(request) != types.ActionContinue {
					t.Fatal("blocked")
				}
				if !tc.enabled && !reflect.DeepEqual(request, host.GetRequestHeaders()) {
					t.Fatal("skipped request mutated")
				}
				// Later path/config changes must not change the recorded response decision.
				r.cfg.IncludePathPrefixes = nil
				r.cfg.ExcludePathPrefixes = []string{"/"}
				response := [][2]string{{":status", "200"}, {"set-cookie", "PC_AUTH_TOKEN=new; HttpOnly"}, {"x-gray-user", "upstream"}, {"x-frontend-gray-refresh", "upstream"}}
				if tc.enabled {
					response = response[:3]
				}
				host.CallOnHttpResponseHeaders(response)
				if !tc.enabled {
					if !reflect.DeepEqual(response, host.GetResponseHeaders()) {
						t.Fatal("skipped response mutated")
					}
				} else {
					foundStage, foundRefresh, foundCookie, foundLogin := false, false, false, false
					for _, h := range host.GetResponseHeaders() {
						if h[0] == "x-gray-user" && h[1] == "canary" {
							foundStage = true
						}
						if h[0] == "x-frontend-gray-refresh" && h[1] == "true" {
							foundRefresh = true
						}
						if h[0] == "set-cookie" || h[0] == "Set-Cookie" {
							// The SDK mock concatenates repeated response header values.
							if strings.Contains(h[1], "PC_AUTH_TOKEN=new; HttpOnly") {
								foundLogin = true
							}
							if strings.Contains(h[1], "x-gray-frontend=always;") {
								foundCookie = true
							}
						}
					}
					if !foundStage || !foundRefresh || !foundCookie || !foundLogin {
						t.Fatalf("included response behavior changed: %v", host.GetResponseHeaders())
					}
				}
				if len(host.GetRedisCalloutAttributes()) != 0 {
					t.Fatal("request issued Redis call")
				}
			})
		})
	}
}
