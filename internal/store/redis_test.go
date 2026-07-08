package store

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/BlackDark/renovate-server/internal/config"
)

func newTestRedis(t *testing.T) Store {
	t.Helper()
	mr := miniredis.RunT(t)
	s, err := NewRedis(t.Context(), config.RedisConfig{
		URL: "redis://" + mr.Addr(), KeyPrefix: "test:", TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRedisSemantics(t *testing.T) {
	testStoreSemantics(t, newTestRedis(t))
}

func TestRedisTTLSet(t *testing.T) {
	mr := miniredis.RunT(t)
	s, err := NewRedis(t.Context(), config.RedisConfig{
		URL: "redis://" + mr.Addr(), KeyPrefix: "test:", TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Queue("gl:a", "push")
	if ttl := mr.TTL("test:repo:gl:a"); ttl <= 0 || ttl > time.Hour {
		t.Fatalf("ttl = %v, want (0, 1h]", ttl)
	}
}

func TestRedisHandleTTLSet(t *testing.T) {
	mr := miniredis.RunT(t)
	s, err := NewRedis(t.Context(), config.RedisConfig{
		URL: "redis://" + mr.Addr(), KeyPrefix: "test:", TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.SaveRunHandle("gl:a", "data")
	if ttl := mr.TTL("test:handle:gl:a"); ttl <= 0 || ttl > time.Hour {
		t.Fatalf("handle ttl = %v, want (0, 1h]", ttl)
	}
}

func TestRedisConnectFailure(t *testing.T) {
	_, err := NewRedis(t.Context(), config.RedisConfig{
		URL: "redis://127.0.0.1:1", KeyPrefix: "test:", TTL: time.Hour,
	})
	if err == nil {
		t.Fatal("want connection error")
	}
}

func TestRedisBadURL(t *testing.T) {
	_, err := NewRedis(t.Context(), config.RedisConfig{URL: "://bogus"})
	if err == nil {
		t.Fatal("want parse error")
	}
}
