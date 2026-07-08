package store

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/BlackDark/renovate-server/internal/config"
)

const redisOpTimeout = 5 * time.Second

// Lua scripts keep each state transition atomic. Every write refreshes the
// key TTL so stale entries (e.g. after an unclean shutdown) self-heal.
var (
	queueScript = redis.NewScript(`
local state = redis.call('HGET', KEYS[1], 'state')
if not state then
  redis.call('HSET', KEYS[1], 'state', 'queued', 'reason', ARGV[1], 'since', ARGV[2], 'rerun', '')
  redis.call('EXPIRE', KEYS[1], ARGV[3])
  return 0
end
redis.call('EXPIRE', KEYS[1], ARGV[3])
if state == 'running' then
  redis.call('HSET', KEYS[1], 'rerun', '1')
  return 2
end
return 1
`)

	startRunScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then
  redis.call('HSET', KEYS[1], 'state', 'running', 'since', ARGV[1])
  redis.call('EXPIRE', KEYS[1], ARGV[2])
end
return 0
`)

	finishRunScript = redis.NewScript(`
local rerun = redis.call('HGET', KEYS[1], 'rerun')
redis.call('DEL', KEYS[1])
if rerun == '1' then
  return 1
end
return 0
`)

	adoptScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then
  redis.call('HSET', KEYS[1], 'state', 'running', 'reason', ARGV[1], 'since', ARGV[2], 'rerun', '')
end
redis.call('EXPIRE', KEYS[1], ARGV[3])
return 0
`)
)

type redisStore struct {
	client *redis.Client
	prefix string
	ttl    time.Duration
}

// NewRedis returns a redis-backed Store. State survives restarts, so
// queued/running markers can be recovered at startup; entries expire after
// cfg.TTL as a stale-lock safety net.
func NewRedis(ctx context.Context, cfg config.RedisConfig) (Store, error) {
	opts, err := redis.ParseURL(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	client := redis.NewClient(opts)
	pingCtx, cancel := context.WithTimeout(ctx, redisOpTimeout)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		return nil, fmt.Errorf("connect to redis: %w", err)
	}
	return &redisStore{client: client, prefix: cfg.KeyPrefix, ttl: cfg.TTL}, nil
}

func (r *redisStore) key(repoKey string) string { return r.prefix + "repo:" + repoKey }

func (r *redisStore) ttlSeconds() int64 { return int64(r.ttl.Seconds()) }

func (r *redisStore) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), redisOpTimeout)
}

func (r *redisStore) Queue(repoKey, reason string) QueueResult {
	ctx, cancel := r.ctx()
	defer cancel()
	res, err := queueScript.Run(ctx, r.client, []string{r.key(repoKey)},
		reason, time.Now().Format(time.RFC3339Nano), r.ttlSeconds()).Int()
	if err != nil {
		// Failing open would double-run repos; failing closed only delays a
		// run until the next event or cron tick. Fail closed.
		slog.Error("redis queue operation failed, dropping event", "repo", repoKey, "error", err)
		return Coalesced
	}
	switch res {
	case 0:
		return Queued
	case 2:
		return Deferred
	default:
		return Coalesced
	}
}

func (r *redisStore) StartRun(repoKey string) {
	ctx, cancel := r.ctx()
	defer cancel()
	if err := startRunScript.Run(ctx, r.client, []string{r.key(repoKey)},
		time.Now().Format(time.RFC3339Nano), r.ttlSeconds()).Err(); err != nil {
		slog.Error("redis start-run operation failed", "repo", repoKey, "error", err)
	}
}

func (r *redisStore) FinishRun(repoKey string) bool {
	ctx, cancel := r.ctx()
	defer cancel()
	res, err := finishRunScript.Run(ctx, r.client, []string{r.key(repoKey)}).Int()
	if err != nil {
		slog.Error("redis finish-run operation failed", "repo", repoKey, "error", err)
		return false
	}
	return res == 1
}

func (r *redisStore) Adopt(repoKey, reason string) {
	ctx, cancel := r.ctx()
	defer cancel()
	if err := adoptScript.Run(ctx, r.client, []string{r.key(repoKey)},
		reason, time.Now().Format(time.RFC3339Nano), r.ttlSeconds()).Err(); err != nil {
		slog.Error("redis adopt operation failed", "repo", repoKey, "error", err)
	}
}

func (r *redisStore) handleKey(repoKey string) string { return r.prefix + "handle:" + repoKey }

func (r *redisStore) SaveRunHandle(key, data string) {
	ctx, cancel := r.ctx()
	defer cancel()
	if err := r.client.Set(ctx, r.handleKey(key), data, r.ttl).Err(); err != nil {
		slog.Error("redis save-handle operation failed", "repo", key, "error", err)
	}
}

func (r *redisStore) LoadRunHandles() map[string]string {
	ctx, cancel := r.ctx()
	defer cancel()
	out := map[string]string{}
	iter := r.client.Scan(ctx, 0, r.prefix+"handle:*", 100).Iterator()
	for iter.Next(ctx) {
		key := iter.Val()
		data, err := r.client.Get(ctx, key).Result()
		if err != nil {
			continue
		}
		out[strings.TrimPrefix(key, r.prefix+"handle:")] = data
	}
	if err := iter.Err(); err != nil {
		slog.Error("redis handle scan failed", "error", err)
	}
	return out
}

func (r *redisStore) DeleteRunHandle(key string) {
	ctx, cancel := r.ctx()
	defer cancel()
	if err := r.client.Del(ctx, r.handleKey(key)).Err(); err != nil {
		slog.Error("redis delete-handle operation failed", "repo", key, "error", err)
	}
}

func (r *redisStore) Snapshot() map[string]RepoStatus {
	ctx, cancel := r.ctx()
	defer cancel()
	out := map[string]RepoStatus{}
	iter := r.client.Scan(ctx, 0, r.prefix+"repo:*", 100).Iterator()
	for iter.Next(ctx) {
		key := iter.Val()
		fields, err := r.client.HGetAll(ctx, key).Result()
		if err != nil || len(fields) == 0 {
			continue
		}
		since, _ := time.Parse(time.RFC3339Nano, fields["since"])
		out[strings.TrimPrefix(key, r.prefix+"repo:")] = RepoStatus{
			State:        State(fields["state"]),
			Reason:       fields["reason"],
			Since:        since,
			PendingRerun: fields["rerun"] == "1",
		}
	}
	if err := iter.Err(); err != nil {
		slog.Error("redis snapshot scan failed", "error", err)
	}
	return out
}
