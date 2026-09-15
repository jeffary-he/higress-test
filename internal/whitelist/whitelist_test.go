package whitelist

import (
	"strings"
	"testing"
	"time"
)

func TestDigestContract(t *testing.T) {
	const abc = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	for _, input := range []string{"abc", "Bearer abc"} {
		got, ok := Digest(input)
		if !ok || got != abc {
			t.Fatalf("digest contract failed")
		}
	}
	for _, input := range []string{"", "Bearer "} {
		if _, ok := Digest(input); ok {
			t.Fatal("empty token accepted")
		}
	}
	for _, input := range []string{"ABC", "bearer abc", "Bearer  abc", "abc\n", "abc "} {
		got, ok := Digest(input)
		if !ok || got == abc {
			t.Fatal("token content changed unexpectedly")
		}
	}
	for _, value := range []string{"", "abc", strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		if ValidDigest(value) {
			t.Fatal("invalid digest accepted")
		}
	}
	if !ValidDigest(abc) {
		t.Fatal("valid digest rejected")
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
		t.Fatal("empty snapshot did not remove old tokens")
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
