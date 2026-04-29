// Package cache holds the response-cache layer for /v1/chat/completions.
//
// Week 9 ships exact-match caching: identical (model, messages, sampling
// params) → identical key → byte-equal cached response. Week 10 layers
// semantic similarity on top.
//
// Pollution prevention is the caller's job (see chat handler): only 200
// responses with finish_reason=stop, non-empty content, and a usage
// block reach Set(). The cache itself stores whatever bytes it's given.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/An-idd/x-beacon/internal/provider"
)

// ErrMiss is returned by Get when the key is absent. Wrapped errors from
// the backend (redis down, decode failure) surface as their original
// concrete type — callers fail-open on those.
var ErrMiss = errors.New("cache: miss")

// Exact is the exact-match response cache contract. Get/Set/Delete are
// best-effort: a backend error never propagates as a fatal failure to
// the chat hot path. The middleware logs and treats it as a miss.
type Exact interface {
	Get(ctx context.Context, key string) (*provider.ChatResponse, error)
	Set(ctx context.Context, key string, resp *provider.ChatResponse, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
}

// Key derives the canonical cache key for a chat request.
//
// Inputs hashed: model, messages (chronological order matters), and the
// sampling-param whitelist {temperature, top_p, max_tokens, stop}. The
// stream flag is intentionally excluded — Week 9 streaming bypasses the
// cache anyway, and Week 10 will replay cached responses as a synthetic
// stream so the same key serves both shapes.
//
// Returns hex-encoded sha256 prefixed with "cache:exact:". Same logical
// request → same key, byte-for-byte.
func Key(req *provider.ChatRequest) (string, error) {
	if req == nil {
		return "", errors.New("cache: nil request")
	}
	payload := cacheKeyPayload{
		Model:       req.Model,
		Messages:    req.Messages,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		MaxTokens:   req.MaxTokens,
		Stop:        req.Stop,
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("cache: marshal key payload: %w", err)
	}
	sum := sha256.Sum256(buf)
	return "cache:exact:" + hex.EncodeToString(sum[:]), nil
}

// cacheKeyPayload is the deterministic shape we hash. Field tags use
// omitempty so default-zero values don't shift the hash when a client
// omits them (e.g. temperature=nil hashes the same with or without the
// key). encoding/json emits struct fields in declaration order, giving
// us deterministic output without sort.
type cacheKeyPayload struct {
	Model       string             `json:"model"`
	Messages    []provider.Message `json:"messages"`
	Temperature *float64           `json:"temperature,omitempty"`
	TopP        *float64           `json:"top_p,omitempty"`
	MaxTokens   int                `json:"max_tokens,omitempty"`
	Stop        []string           `json:"stop,omitempty"`
}

// RedisExact persists cached responses in Redis as JSON-encoded bytes
// keyed by Key(). One Redis round-trip per Get / Set.
type RedisExact struct {
	client *redis.Client
}

// NewRedisExact returns an Exact backed by the given Redis client. The
// client must be ready; nil here is a programming error.
func NewRedisExact(client *redis.Client) *RedisExact {
	if client == nil {
		panic("cache: NewRedisExact requires a non-nil redis client")
	}
	return &RedisExact{client: client}
}

// Get returns the cached response or ErrMiss. Backend errors (network,
// decode) are returned as-is and the middleware treats them as a miss.
func (r *RedisExact) Get(ctx context.Context, key string) (*provider.ChatResponse, error) {
	raw, err := r.client.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, ErrMiss
		}
		return nil, fmt.Errorf("cache: redis get: %w", err)
	}
	var resp provider.ChatResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		// Treat malformed cached entries as a miss — they're stale and
		// will be overwritten by the next successful upstream call.
		return nil, fmt.Errorf("cache: decode entry: %w", err)
	}
	return &resp, nil
}

// Set writes the cached response with the given TTL. ttl <= 0 is
// rejected so we never accidentally write a never-expiring entry.
func (r *RedisExact) Set(ctx context.Context, key string, resp *provider.ChatResponse, ttl time.Duration) error {
	if resp == nil {
		return errors.New("cache: Set: nil response")
	}
	if ttl <= 0 {
		return fmt.Errorf("cache: Set: non-positive ttl %v", ttl)
	}
	buf, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("cache: marshal response: %w", err)
	}
	if err := r.client.Set(ctx, key, buf, ttl).Err(); err != nil {
		return fmt.Errorf("cache: redis set: %w", err)
	}
	return nil
}

// Delete is best-effort. Missing keys are not an error.
func (r *RedisExact) Delete(ctx context.Context, key string) error {
	if err := r.client.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("cache: redis del: %w", err)
	}
	return nil
}
