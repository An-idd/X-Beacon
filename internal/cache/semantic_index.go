package cache

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

// SemanticIndex is the contract Week 10 plugs in front of the chat
// handler. RediSearch HNSW is the only Week 9 implementation; the
// interface keeps it possible to swap in pgvector or qdrant later
// without touching callers.
//
// Insert / Delete / Search are best-effort: a backend error never
// propagates as a fatal failure to the chat hot path. Week 10 will
// log + treat as miss the same way exact does.
type SemanticIndex interface {
	// EnsureIndex creates the underlying RediSearch index on first
	// call. Idempotent — re-running against an existing index is a
	// no-op (the "Index already exists" error is swallowed).
	EnsureIndex(ctx context.Context) error

	// Insert stores one (key, vector, payload) tuple. payload is the
	// opaque blob (typically JSON-encoded ChatResponse) returned on
	// search hit. Vector length must match the index dimension.
	Insert(ctx context.Context, key string, vec []float32, payload []byte) error

	// Search returns the top-K closest entries by cosine distance, in
	// ascending order (smallest distance first). topK <= 0 returns
	// ErrInvalidTopK.
	Search(ctx context.Context, vec []float32, topK int) ([]SemanticMatch, error)

	// Delete removes one entry. Missing key is not an error.
	Delete(ctx context.Context, key string) error
}

// SemanticMatch is one hit from Search. Score is cosine distance
// (0=identical, 2=opposite). Convert to similarity with `1 - Score/2`
// or compare distance against threshold directly.
type SemanticMatch struct {
	Key     string
	Score   float64
	Payload []byte
}

// ErrInvalidTopK is returned by Search when topK <= 0.
var ErrInvalidTopK = errors.New("cache: topK must be > 0")

// ErrDimensionMismatch is returned when a vector's length differs
// from the index's configured dimension.
var ErrDimensionMismatch = errors.New("cache: vector dimension mismatch")

// SemanticIndexConfig parameterizes RedisSemanticIndex. Index /
// KeyPrefix / Dimensions are set at construction; dimension changes
// require a fresh index name (RediSearch can't ALTER schema).
type SemanticIndexConfig struct {
	// IndexName is the RediSearch FT index id. Must be unique per
	// (model, dim) tuple if a deployment runs multiple — using the
	// same name across dimensions silently breaks search. Default
	// "x_beacon_semcache".
	IndexName string

	// KeyPrefix is the HSET key prefix for entries. Default
	// "cache:sem:". The same prefix is given to FT.CREATE PREFIX so
	// the index only sees entries we own.
	KeyPrefix string

	// Dimensions is the vector length, must match the embedding
	// model's output. Required (no default — silent dim mismatch is
	// the worst possible failure mode).
	Dimensions int

	// Distance is the metric RediSearch uses. Default "COSINE";
	// "L2" / "IP" also valid.
	Distance string

	// HNSWM and HNSWEFConstruction are HNSW knobs. Defaults are
	// RediSearch's own (16 / 200) — only override after measuring.
	HNSWM              int
	HNSWEFConstruction int
}

const (
	defaultIndexName          = "x_beacon_semcache"
	defaultKeyPrefix          = "cache:sem:"
	defaultDistance           = "COSINE"
	defaultHNSWM              = 16
	defaultHNSWEFConstruction = 200

	hashFieldVector  = "vec"
	hashFieldPayload = "payload"
	hashFieldKey     = "cache_key"
)

// RedisSemanticIndex is the RediSearch-HNSW-backed SemanticIndex.
// Requires a redis-stack-server (or a Redis with the search module
// loaded); plain redis:7-alpine will reject the FT.* commands.
type RedisSemanticIndex struct {
	client *redis.Client
	cfg    SemanticIndexConfig
}

// NewRedisSemanticIndex validates cfg and returns the index. Does
// NOT call EnsureIndex — caller decides startup ordering.
func NewRedisSemanticIndex(client *redis.Client, cfg SemanticIndexConfig) (*RedisSemanticIndex, error) {
	if client == nil {
		return nil, errors.New("cache: NewRedisSemanticIndex requires a non-nil redis client")
	}
	if cfg.Dimensions <= 0 {
		return nil, fmt.Errorf("cache: SemanticIndexConfig.Dimensions must be > 0, got %d", cfg.Dimensions)
	}
	if cfg.IndexName == "" {
		cfg.IndexName = defaultIndexName
	}
	if cfg.KeyPrefix == "" {
		cfg.KeyPrefix = defaultKeyPrefix
	}
	if cfg.Distance == "" {
		cfg.Distance = defaultDistance
	}
	if cfg.HNSWM <= 0 {
		cfg.HNSWM = defaultHNSWM
	}
	if cfg.HNSWEFConstruction <= 0 {
		cfg.HNSWEFConstruction = defaultHNSWEFConstruction
	}
	return &RedisSemanticIndex{client: client, cfg: cfg}, nil
}

// EnsureIndex runs FT.CREATE. The "Index already exists" error is
// swallowed so callers can invoke this at every startup. Other
// errors (module not loaded, malformed args) propagate.
func (r *RedisSemanticIndex) EnsureIndex(ctx context.Context) error {
	args := r.createArgs()
	if err := r.client.Do(ctx, args...).Err(); err != nil {
		// RediSearch returns this exact string on duplicate; treat as
		// success. Substring match is intentional — the prefix may be
		// localized in some upstream forks.
		if strings.Contains(strings.ToLower(err.Error()), "index already exists") {
			return nil
		}
		return fmt.Errorf("cache: FT.CREATE failed: %w", err)
	}
	return nil
}

// createArgs builds the FT.CREATE argument list. Pure function so
// tests can verify the wire shape without a Redis instance.
func (r *RedisSemanticIndex) createArgs() []any {
	return []any{
		"FT.CREATE", r.cfg.IndexName,
		"ON", "HASH",
		"PREFIX", "1", r.cfg.KeyPrefix,
		"SCHEMA",
		hashFieldVector, "VECTOR", "HNSW", "10",
		"TYPE", "FLOAT32",
		"DIM", strconv.Itoa(r.cfg.Dimensions),
		"DISTANCE_METRIC", r.cfg.Distance,
		"M", strconv.Itoa(r.cfg.HNSWM),
		"EF_CONSTRUCTION", strconv.Itoa(r.cfg.HNSWEFConstruction),
	}
}

// Insert HSETs the vector + payload + key under the configured prefix.
// Calling Insert with an existing key overwrites the previous entry
// (RediSearch picks up the change automatically).
func (r *RedisSemanticIndex) Insert(ctx context.Context, key string, vec []float32, payload []byte) error {
	if len(vec) != r.cfg.Dimensions {
		return fmt.Errorf("%w: want %d, got %d", ErrDimensionMismatch, r.cfg.Dimensions, len(vec))
	}
	hashKey := r.cfg.KeyPrefix + key
	encoded := EncodeVector(vec)
	if err := r.client.HSet(ctx, hashKey, map[string]any{
		hashFieldVector:  encoded,
		hashFieldPayload: payload,
		hashFieldKey:     key,
	}).Err(); err != nil {
		return fmt.Errorf("cache: HSET %s: %w", hashKey, err)
	}
	return nil
}

// Search runs a KNN query against the index. Results come back in
// ascending distance order. The hash payload + cache_key fields are
// returned so callers don't need a second round-trip.
func (r *RedisSemanticIndex) Search(ctx context.Context, vec []float32, topK int) ([]SemanticMatch, error) {
	if topK <= 0 {
		return nil, ErrInvalidTopK
	}
	if len(vec) != r.cfg.Dimensions {
		return nil, fmt.Errorf("%w: want %d, got %d", ErrDimensionMismatch, r.cfg.Dimensions, len(vec))
	}

	args := r.searchArgs(vec, topK)
	raw, err := r.client.Do(ctx, args...).Result()
	if err != nil {
		return nil, fmt.Errorf("cache: FT.SEARCH failed: %w", err)
	}
	return parseSearchReply(raw)
}

// searchArgs builds the FT.SEARCH command. The DIALECT 2 + PARAMS
// pattern is the modern RediSearch way to pass binary vectors; the
// blob lives in the PARAMS section, the query references it by name.
func (r *RedisSemanticIndex) searchArgs(vec []float32, topK int) []any {
	encoded := EncodeVector(vec)
	query := fmt.Sprintf("*=>[KNN %d @%s $vec_param AS score]", topK, hashFieldVector)
	return []any{
		"FT.SEARCH", r.cfg.IndexName, query,
		"PARAMS", "2", "vec_param", encoded,
		"RETURN", "3", "score", hashFieldPayload, hashFieldKey,
		"SORTBY", "score",
		"DIALECT", "2",
		"LIMIT", "0", strconv.Itoa(topK),
	}
}

// Delete removes one entry. Missing keys produce DEL=0 which we
// treat as success (idempotent semantics, same as RedisExact).
func (r *RedisSemanticIndex) Delete(ctx context.Context, key string) error {
	hashKey := r.cfg.KeyPrefix + key
	if err := r.client.Del(ctx, hashKey).Err(); err != nil {
		return fmt.Errorf("cache: DEL %s: %w", hashKey, err)
	}
	return nil
}

// EncodeVector serializes a float32 slice to little-endian bytes.
// RediSearch consumes this layout directly when the schema declares
// TYPE FLOAT32. Exposed for tests + callers that need to send
// pre-encoded blobs through other paths.
func EncodeVector(vec []float32) []byte {
	buf := make([]byte, 4*len(vec))
	for i, f := range vec {
		binary.LittleEndian.PutUint32(buf[4*i:4*(i+1)], math.Float32bits(f))
	}
	return buf
}

// DecodeVector is the inverse of EncodeVector. Exists for tests and
// debugging tools; the gateway hot path never reads vectors back.
func DecodeVector(buf []byte) ([]float32, error) {
	if len(buf)%4 != 0 {
		return nil, fmt.Errorf("cache: vector buffer length %d is not a multiple of 4", len(buf))
	}
	out := make([]float32, len(buf)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[4*i : 4*(i+1)]))
	}
	return out, nil
}

// parseSearchReply unwraps RediSearch FT.SEARCH's reply. Two shapes
// in the wild:
//
//   - RESP2 / older modules: flat array
//     [total, key1, [field1, val1, ...], key2, [...], ...]
//   - RESP3 / redis-stack 7.4+: map
//     {total_results, results: [{id, extra_attributes:{...}, values}, ...]}
//
// go-redis picks the protocol per-connection, so we accept both rather
// than pin a wire version. Off-shape replies are an alert signal, not
// a miss — they propagate as errors.
func parseSearchReply(raw any) ([]SemanticMatch, error) {
	switch v := raw.(type) {
	case []any:
		return parseSearchReplyArray(v)
	case map[any]any:
		return parseSearchReplyMap(v)
	case map[string]any:
		// Defensive — some clients normalize string-keyed maps.
		conv := make(map[any]any, len(v))
		for k, vv := range v {
			conv[k] = vv
		}
		return parseSearchReplyMap(conv)
	default:
		return nil, fmt.Errorf("cache: FT.SEARCH unexpected top-level type %T", raw)
	}
}

func parseSearchReplyArray(arr []any) ([]SemanticMatch, error) {
	if len(arr) == 0 {
		return nil, nil
	}
	matches := make([]SemanticMatch, 0, (len(arr)-1)/2)
	for i := 1; i+1 < len(arr); i += 2 {
		key, _ := arr[i].(string)
		fields, ok := arr[i+1].([]any)
		if !ok {
			continue
		}
		m := SemanticMatch{Key: key}
		for j := 0; j+1 < len(fields); j += 2 {
			fname, _ := fields[j].(string)
			switch fname {
			case "score":
				m.Score = parseScore(fields[j+1])
			case hashFieldPayload:
				m.Payload = toBytes(fields[j+1])
			case hashFieldKey:
				if v, ok := fields[j+1].(string); ok {
					m.Key = v
				}
			}
		}
		matches = append(matches, m)
	}
	return matches, nil
}

func parseSearchReplyMap(m map[any]any) ([]SemanticMatch, error) {
	resultsAny, ok := m["results"]
	if !ok {
		// "0 results" can shape as {total_results:0} with no results
		// key on some module versions.
		if total, ok := m["total_results"]; ok {
			if parseScore(total) == 0 {
				return nil, nil
			}
		}
		return nil, fmt.Errorf("cache: FT.SEARCH map reply missing 'results' field")
	}
	results, ok := resultsAny.([]any)
	if !ok {
		return nil, fmt.Errorf("cache: FT.SEARCH 'results' is %T, want array", resultsAny)
	}
	matches := make([]SemanticMatch, 0, len(results))
	for _, entry := range results {
		em, ok := entry.(map[any]any)
		if !ok {
			continue
		}
		var match SemanticMatch
		if id, ok := em["id"].(string); ok {
			match.Key = id
		}
		attrsAny, _ := em["extra_attributes"]
		switch attrs := attrsAny.(type) {
		case map[any]any:
			for k, v := range attrs {
				ks, _ := k.(string)
				switch ks {
				case "score":
					match.Score = parseScore(v)
				case hashFieldPayload:
					match.Payload = toBytes(v)
				case hashFieldKey:
					if s, ok := v.(string); ok {
						match.Key = s
					}
				}
			}
		case []any:
			// Some modules emit attrs as flat array even under RESP3.
			for j := 0; j+1 < len(attrs); j += 2 {
				k, _ := attrs[j].(string)
				switch k {
				case "score":
					match.Score = parseScore(attrs[j+1])
				case hashFieldPayload:
					match.Payload = toBytes(attrs[j+1])
				case hashFieldKey:
					if s, ok := attrs[j+1].(string); ok {
						match.Key = s
					}
				}
			}
		}
		matches = append(matches, match)
	}
	return matches, nil
}

func parseScore(v any) float64 {
	switch x := v.(type) {
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	case float64:
		return x
	case int64:
		return float64(x)
	}
	return 0
}

func toBytes(v any) []byte {
	switch x := v.(type) {
	case string:
		return []byte(x)
	case []byte:
		return x
	}
	return nil
}
