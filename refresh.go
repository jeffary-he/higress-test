package main

import (
	"crypto/sha256"
	"encoding/binary"
	"strconv"
	"time"

	"github.com/higress-group/proxy-wasm-go-sdk/proxywasm"
	"github.com/higress-group/wasm-go/pkg/wrapper"
	"github.com/tidwall/resp"
	"gray-whitelist-wasm/internal/whitelist"
)

// Published snapshots and their maps are immutable. Envoy serializes callbacks
// within a VM; publishing one pointer prevents partially applied mode changes.
type graySnapshot struct {
	enabled  bool
	mode     string
	percent  int
	tenants  map[string]struct{}
	users    map[string]struct{}
	loadedAt time.Time
}

func (r *runtimeState) stage(headers [][2]string) string {
	s := r.snapshot
	now := r.now()
	if s == nil || !s.enabled || now.Before(s.loadedAt) || now.Sub(s.loadedAt) >= time.Duration(r.cfg.TTL)*time.Millisecond {
		return "stable"
	}
	if s.mode == "traffic" {
		token, count := findCookie(headers, r.cfg.TokenCookieName)
		if count != 1 {
			return "stable"
		}
		return trafficStage(token, s.percent)
	}
	if s.mode != "whitelist" {
		return "stable"
	}
	return resolveStage(headers, r.cfg.TokenCookieName, r.cfg.TenantIDClaim, r.cfg.UserIDClaim, func(identity string) bool {
		_, tenant := s.tenants[identity]
		_, user := s.users[identity]
		return tenant || user
	})
}

func trafficStage(token string, percent int) string {
	if token == "" || len(token) > 16384 || percent < 0 || percent > 100 {
		return "stable"
	}
	digest := sha256.Sum256([]byte(token))
	slot := binary.BigEndian.Uint64(digest[:8]) % 100
	if slot < uint64(percent) {
		return "canary"
	}
	return "stable"
}

func parsePercent(v resp.Value) (int, bool) {
	if v.Error() != nil || v.Type() != resp.BulkString || v.IsNull() {
		return 0, false
	}
	n, err := strconv.Atoi(v.String())
	return n, err == nil && n >= 0 && n <= 100 && strconv.Itoa(n) == v.String()
}

// Each refresh reads only its selected branch. Failed or late callbacks never
// publish a partial snapshot or renew the previous snapshot's TTL.
func (r *runtimeState) refresh() {
	now := r.now()
	if r.refreshing && now.Before(r.refreshDeadline) {
		return
	}
	r.refreshGeneration++
	id := r.refreshGeneration
	r.refreshing = true
	// At most four sequential operations, each with the configured host timeout.
	r.refreshDeadline = now.Add(4 * time.Duration(r.cfg.Timeout) * time.Millisecond)
	next := &graySnapshot{}
	active := func() bool {
		return r.refreshing && id == r.refreshGeneration && !r.now().Before(now) && r.now().Before(r.refreshDeadline)
	}
	fail := func() {
		if id == r.refreshGeneration {
			r.refreshing = false
			proxywasm.LogWarn("gray-whitelist: configuration refresh rejected; retaining previous snapshot until TTL")
		}
	}
	publish := func() {
		if !active() {
			return
		}
		next.loadedAt = r.now()
		r.snapshot = next
		r.refreshing = false
		proxywasm.LogInfof("gray-whitelist: configuration refreshed; enabled=%t mode=%s percent=%d tenants=%d users=%d",
			next.enabled, next.mode, next.percent, len(next.tenants), len(next.users))
	}
	read := func(set bool, key string, accept func(resp.Value)) {
		if !active() {
			return
		}
		// A dispatched operation owns one callback; guard against duplicate or
		// stale completion before it can continue the refresh chain.
		completed := false
		callback := wrapper.RedisResponseCallback(func(v resp.Value) {
			if completed || !active() {
				return
			}
			completed = true
			if v.Error() != nil {
				fail()
				return
			}
			accept(v)
		})
		var err error
		if set {
			err = r.client.SMembers(key, callback)
		} else {
			err = r.client.Get(key, callback)
		}
		if err != nil && !completed {
			completed = true
			fail()
		}
	}
	read(false, r.cfg.EnabledKey, func(v resp.Value) {
		if v.Type() != resp.BulkString || v.IsNull() || (v.String() != "0" && v.String() != "1") {
			fail()
			return
		}
		next.enabled = v.String() == "1"
		if !next.enabled {
			publish()
			return
		}
		read(false, r.cfg.ModeKey, func(v resp.Value) {
			// Missing mode preserves compatibility with 0.6.5 Redis data.
			if v.Type() != resp.BulkString {
				fail()
				return
			}
			next.mode = v.String()
			if v.IsNull() {
				next.mode = "whitelist"
			}
			switch next.mode {
			case "traffic":
				read(false, r.cfg.PercentKey, func(v resp.Value) {
					n, ok := parsePercent(v)
					if !ok {
						fail()
						return
					}
					next.percent = n
					publish()
				})
			case "whitelist":
				read(true, r.cfg.TenantKey, func(v resp.Value) {
					var err error
					next.tenants, err = parseTypedSnapshot(v, r.cfg.MaxEntries, whitelist.TenantSelector)
					if err != nil {
						fail()
						return
					}
					read(true, r.cfg.UserKey, func(v resp.Value) {
						next.users, err = parseTypedSnapshot(v, r.cfg.MaxEntries, whitelist.UserSelector)
						if err != nil {
							fail()
							return
						}
						publish()
					})
				})
			default:
				fail()
			}
		})
	})
}
