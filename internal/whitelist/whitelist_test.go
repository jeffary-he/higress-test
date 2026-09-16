package whitelist

import (
	"testing"
	"time"
)

func TestSelectorContract(t *testing.T) {
	tenant, ok := TenantSelector("2")
	if !ok || tenant != "tenant:2" || !ValidSelector(tenant) {
		t.Fatal("valid tenant rejected")
	}
	user, ok := UserSelector("283778812672")
	if !ok || user != "user:283778812672" || !ValidSelector(user) {
		t.Fatal("valid user rejected")
	}
	for _, raw := range []string{"", "02", "-1", "a", "18446744073709551616"} {
		if _, ok := TenantSelector(raw); ok {
			t.Fatal("invalid tenant accepted")
		}
		if _, ok := UserSelector(raw); ok {
			t.Fatal("invalid user accepted")
		}
	}
	for _, value := range []string{"", "2", "tenant:", "user:", "tenant:02", "user:a", "other:2", "tenant:2:3"} {
		if ValidSelector(value) {
			t.Fatal("invalid Redis member accepted")
		}
	}
}

func TestCacheLifecycle(t *testing.T) {
	now := time.Unix(100, 0)
	c := NewCache(time.Minute)
	if c.Contains("a", now) {
		t.Fatal("cold cache hit")
	}
	id, _ := c.Begin(now, time.Second)
	if _, ok := c.Begin(now, time.Second); ok {
		t.Fatal("overlapping refresh")
	}
	if !c.Finish(id, now, now, map[string]struct{}{"a": {}}, true) {
		t.Fatal("initial load")
	}
	failAt := now.Add(10 * time.Second)
	id, _ = c.Begin(failAt, time.Second)
	c.Finish(id, failAt, failAt, nil, false)
	if !c.Contains("a", now.Add(59*time.Second)) {
		t.Fatal("failure discarded valid cache")
	}
	if c.Contains("a", now.Add(time.Minute)) {
		t.Fatal("failure extended TTL")
	}
	recovery := now.Add(70 * time.Second)
	id, _ = c.Begin(recovery, time.Second)
	c.Finish(id, recovery, recovery, map[string]struct{}{"b": {}}, true)
	if c.Contains("a", recovery) || !c.Contains("b", recovery) {
		t.Fatal("snapshot not replaced")
	}
	id, _ = c.Begin(recovery, time.Second)
	c.Finish(id, recovery, recovery, map[string]struct{}{}, true)
	if c.Contains("b", recovery) {
		t.Fatal("empty snapshot did not remove old identities")
	}
}

func TestCacheRejectsLateCallbacks(t *testing.T) {
	now := time.Unix(100, 0)
	c := NewCache(time.Minute)
	old, _ := c.Begin(now, time.Second)
	next := now.Add(2 * time.Second)
	current, ok := c.Begin(next, time.Second)
	if !ok {
		t.Fatal("lost callback prevented retry")
	}
	if c.Finish(old, now, next, map[string]struct{}{"old": {}}, true) {
		t.Fatal("late callback accepted")
	}
	if !c.Finish(current, next, next, map[string]struct{}{"new": {}}, true) {
		t.Fatal("current callback rejected")
	}
	id, _ := c.Begin(next, time.Second)
	if c.Finish(id, next, next.Add(time.Second), map[string]struct{}{"expired": {}}, true) {
		t.Fatal("expired response accepted")
	}
	if !c.Contains("new", next.Add(time.Second)) {
		t.Fatal("late failure corrupted snapshot")
	}
}
