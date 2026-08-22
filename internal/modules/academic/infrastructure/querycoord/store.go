package querycoord

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/LDouble/campus-academic/internal/core/configcenter"
	"github.com/LDouble/campus-academic/internal/modules/academic/application"
	"github.com/klauspost/compress/zstd"
	"github.com/redis/go-redis/v9"
)

const (
	cacheVersionV1      = 1
	cacheVersionV2      = 2
	cacheVersion        = 3
	cacheEncodingZstd   = "zstd"
	maxCachePayloadSize = 8 * 1024 * 1024
	keyPrefix           = "academic-query:v1:"
)

var (
	releaseLeaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)
	rateLimitScript = redis.NewScript(`
local current = redis.call("TIME")
local now = tonumber(current[1]) * 1000 + math.floor(tonumber(current[2]) / 1000)
local rate = tonumber(ARGV[1])
local capacity = tonumber(ARGV[2])
local values = redis.call("HMGET", KEYS[1], "tokens", "updated_at")
local tokens = tonumber(values[1])
local updated_at = tonumber(values[2])
if tokens == nil or updated_at == nil then
  tokens = capacity
  updated_at = now
else
  local elapsed = math.max(0, now - updated_at)
  tokens = math.min(capacity, tokens + elapsed * rate / 1000)
end
local allowed = 0
local retry_after = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
else
  retry_after = math.ceil((1 - tokens) * 1000 / rate)
end
redis.call("HSET", KEYS[1], "tokens", tokens, "updated_at", now)
redis.call("PEXPIRE", KEYS[1], math.ceil(capacity / rate * 2000 + 1000))
return {allowed, retry_after}
`)
	saveCacheScript = redis.NewScript(`
redis.call("SET", KEYS[1], ARGV[1], "PX", ARGV[2])
redis.call("SADD", KEYS[2], KEYS[1])
local index_ttl = redis.call("PTTL", KEYS[2])
local requested_ttl = tonumber(ARGV[2])
if index_ttl < requested_ttl then
  redis.call("PEXPIRE", KEYS[2], requested_ttl)
end
return 1
`)
	circuitStateScript = redis.NewScript(`
local current = redis.call("TIME")
local now = tonumber(current[1]) * 1000 + math.floor(tonumber(current[2]) / 1000)
local open_until = tonumber(redis.call("HGET", KEYS[1], "open_until"))
if open_until ~= nil and open_until > now then
	return {1, open_until - now, 0}
end
local probe_token = redis.call("HGET", KEYS[1], "probe_token")
local probe_until = tonumber(redis.call("HGET", KEYS[1], "probe_until"))
if probe_token and probe_until ~= nil and probe_until > now then
	if ARGV[1] ~= "" and probe_token == ARGV[1] then
		return {0, 0, 0}
	end
	return {1, probe_until - now, 0}
end
if probe_token then
	redis.call("HDEL", KEYS[1], "probe_token", "probe_until")
end
if open_until ~= nil and ARGV[1] ~= "" then
	local requested_probe_ttl = tonumber(ARGV[2])
	local probe_ttl = math.max(1, requested_probe_ttl)
	redis.call("HSET", KEYS[1], "probe_token", ARGV[1], "probe_until", now + probe_ttl)
	local current_ttl = redis.call("PTTL", KEYS[1])
	if current_ttl < probe_ttl then
		redis.call("PEXPIRE", KEYS[1], probe_ttl)
	end
	return {0, 0, 1}
end
return {0, 0, 0}
`)
	recordDeadlineScript = redis.NewScript(`
local current = redis.call("TIME")
local now = tonumber(current[1]) * 1000 + math.floor(tonumber(current[2]) / 1000)
local minimum_samples = tonumber(ARGV[1])
local minimum_deadlines = tonumber(ARGV[2])
local ratio_milli = tonumber(ARGV[3])
local hard_limit = tonumber(ARGV[4])
local hard_window = tonumber(ARGV[5])
local window = tonumber(ARGV[6])
local open_duration = tonumber(ARGV[7])
local completed_at = ARGV[8]
local probe_token = ARGV[9]
local values = redis.call("HMGET", KEYS[1], "open_until", "latest_success_at", "last_failure_at", "probe_token")
local open_until = tonumber(values[1])
local latest_success_at = values[2]
local last_failure_at = values[3]
local active_probe_token = values[4]
if not last_failure_at or completed_at > last_failure_at then
	redis.call("HSET", KEYS[1], "last_failure_at", completed_at)
end
if open_until ~= nil and open_until > now then
	return {1, open_until - now, 0}
end
if active_probe_token and probe_token ~= "" and active_probe_token == probe_token then
	open_until = now + open_duration
	redis.call("HSET", KEYS[1], "open_until", open_until)
	redis.call("HDEL", KEYS[1], "probe_token", "probe_until")
	redis.call("DEL", KEYS[2], KEYS[3])
	redis.call("PEXPIRE", KEYS[1], open_duration + window)
	return {1, open_duration, 1}
end
local window_cutoff = now - window
redis.call("ZREMRANGEBYSCORE", KEYS[2], 0, window_cutoff)
redis.call("ZREMRANGEBYSCORE", KEYS[3], 0, now - math.max(window, hard_window))
local sequence = redis.call("HINCRBY", KEYS[1], "sample_sequence", 1)
local member = tostring(now) .. ":" .. tostring(sequence)
redis.call("ZADD", KEYS[2], now, "sample:" .. member)
redis.call("ZADD", KEYS[3], now, "deadline:" .. member)
local samples = redis.call("ZCARD", KEYS[2])
local deadlines = redis.call("ZCOUNT", KEYS[3], window_cutoff, "+inf")
local hard_deadlines = redis.call("ZCOUNT", KEYS[3], now - hard_window, "+inf")
local event_ttl = math.max(window, hard_window) + open_duration
if hard_deadlines >= hard_limit or (samples >= minimum_samples and deadlines >= minimum_deadlines and deadlines * 1000 >= samples * ratio_milli) then
	open_until = now + open_duration
	redis.call("HSET", KEYS[1], "open_until", open_until)
	redis.call("HDEL", KEYS[1], "probe_token", "probe_until")
	redis.call("DEL", KEYS[2], KEYS[3])
	redis.call("PEXPIRE", KEYS[1], event_ttl)
	return {1, open_duration, 1}
end
redis.call("HDEL", KEYS[1], "open_until")
redis.call("PEXPIRE", KEYS[1], event_ttl)
redis.call("PEXPIRE", KEYS[2], event_ttl)
redis.call("PEXPIRE", KEYS[3], event_ttl)
return {0, 0, 0}
`)
	resetCircuitScript = redis.NewScript(`
local completed_at = ARGV[1]
local window = tonumber(ARGV[2])
local preserve_samples = ARGV[3] == "1"
local hard_window = tonumber(ARGV[4])
local probe_token = ARGV[5]
local values = redis.call("HMGET", KEYS[1], "last_failure_at", "open_until", "probe_token", "latest_success_at")
local last_failure_at = values[1]
local latest_success_at = values[4]
local open_until = tonumber(values[2])
local active_probe_token = values[3]
if not latest_success_at or completed_at > latest_success_at then
	redis.call("HSET", KEYS[1], "latest_success_at", completed_at)
end
local current = redis.call("TIME")
local now = tonumber(current[1]) * 1000 + math.floor(tonumber(current[2]) / 1000)
local probe_success = active_probe_token and probe_token ~= "" and active_probe_token == probe_token
local out_of_order = last_failure_at and completed_at < last_failure_at
local event_ttl = math.max(window, hard_window)
local function preserve_ttl(key, requested_ttl)
	local current_ttl = redis.call("PTTL", key)
	if current_ttl < requested_ttl then
		redis.call("PEXPIRE", key, requested_ttl)
	end
end
local function record_success_sample()
	if not preserve_samples then
		return
	end
	local window_cutoff = now - window
	redis.call("ZREMRANGEBYSCORE", KEYS[2], 0, window_cutoff)
	redis.call("ZREMRANGEBYSCORE", KEYS[3], 0, now - math.max(window, hard_window))
	local sequence = redis.call("HINCRBY", KEYS[1], "sample_sequence", 1)
	local member = tostring(now) .. ":" .. tostring(sequence)
	redis.call("ZADD", KEYS[2], now, "sample:" .. member)
	preserve_ttl(KEYS[1], event_ttl)
	preserve_ttl(KEYS[2], event_ttl)
	preserve_ttl(KEYS[3], event_ttl)
end
if open_until ~= nil and open_until > now then
	record_success_sample()
	return 0
end
if active_probe_token and (probe_token == "" or active_probe_token ~= probe_token) then
	record_success_sample()
	return 0
end
if out_of_order then
	record_success_sample()
	return 0
end
local had_circuit_state = values[1] or values[2] or values[3]
if preserve_samples then
	record_success_sample()
	redis.call("HDEL", KEYS[1], "failures", "last_failure_at")
	if probe_success then
		redis.call("HDEL", KEYS[1], "open_until", "probe_token", "probe_until")
	end
else
	redis.call("HDEL", KEYS[1], "failures", "last_failure_at", "sample_sequence")
	if probe_success then
		redis.call("HDEL", KEYS[1], "open_until", "probe_token", "probe_until")
	end
	redis.call("DEL", KEYS[2], KEYS[3])
	redis.call("PEXPIRE", KEYS[1], window)
end
if had_circuit_state then
	return 1
end
return 0
`)
	releaseCircuitProbeScript = redis.NewScript(`
if redis.call("HGET", KEYS[1], "probe_token") == ARGV[1] then
	redis.call("HDEL", KEYS[1], "probe_token", "probe_until", "open_until")
	return 1
end
return 0
`)
)

type cacheRecordV1 struct {
	Version    int             `json:"version"`
	FreshUntil int64           `json:"fresh_until"`
	StaleUntil int64           `json:"stale_until"`
	Payload    json.RawMessage `json:"payload"`
}

type cacheRecord struct {
	Version    int    `json:"version"`
	Encoding   string `json:"encoding"`
	CachedAt   int64  `json:"cached_at"`
	FreshUntil int64  `json:"fresh_until"`
	StaleUntil int64  `json:"stale_until"`
	Payload    []byte `json:"payload"`
}

type cacheState int

const (
	cacheMiss cacheState = iota
	cacheFresh
	cacheStale
)

type store struct {
	client         *redis.Client
	cipher         *configcenter.Cipher
	fingerprintKey []byte
	encoder        *zstd.Encoder
	decoder        *zstd.Decoder
}

func newStore(client *redis.Client, encryptionKey, fingerprintKey []byte) (*store, error) {
	if client == nil {
		return nil, fmt.Errorf("academic query Redis client is required")
	}
	if len(encryptionKey) != 32 || len(fingerprintKey) != 32 {
		return nil, fmt.Errorf("academic query derived keys must contain exactly 32 bytes")
	}
	cipher, err := configcenter.NewCipher(encryptionKey)
	if err != nil {
		return nil, fmt.Errorf("create academic query cache cipher: %w", err)
	}
	encoder, err := zstd.NewWriter(
		nil,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithEncoderConcurrency(1),
		zstd.WithWindowSize(1<<20),
		zstd.WithLowerEncoderMem(true),
	)
	if err != nil {
		return nil, fmt.Errorf("create academic query cache compressor: %w", err)
	}
	decoder, err := zstd.NewReader(
		nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(maxCachePayloadSize+1),
	)
	if err != nil {
		encoder.Close()
		return nil, fmt.Errorf("create academic query cache decompressor: %w", err)
	}
	return &store{
		client:         client,
		cipher:         cipher,
		fingerprintKey: append([]byte(nil), fingerprintKey...),
		encoder:        encoder,
		decoder:        decoder,
	}, nil
}

func (s *store) digest(parts ...string) string {
	mac := hmac.New(sha256.New, s.fingerprintKey)
	for _, part := range parts {
		// Hash the exact bytes. In particular, credentials must not be
		// normalized: two passwords that differ only by surrounding whitespace
		// are still different credentials and must never share an in-flight call.
		_, _ = mac.Write([]byte(part))
		_, _ = mac.Write([]byte{0})
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *store) resultKey(digest string) string {
	return keyPrefix + "result:" + digest
}

func (s *store) queryLeaseKey(digest string) string {
	return keyPrefix + "lease:query:" + digest
}

func (s *store) studentLeaseKey(digest string) string {
	return keyPrefix + "lease:student:" + digest
}

func (s *store) studentIndexKey(studentNo string) string {
	return keyPrefix + "index:student:" + s.digest("student-index", studentNo)
}

func (s *store) failureKey(digest string) string {
	return keyPrefix + "failure:" + digest
}

func (s *store) circuitKey(operation, educationLevel string) string {
	return keyPrefix + "circuit:" + operation + ":" + s.digest("education-level", educationLevel)
}

func circuitSampleKeys(key string) []string {
	return []string{key, key + ":samples", key + ":deadlines"}
}

func (s *store) failedRecently(ctx context.Context, key string) (string, bool, error) {
	reason, err := s.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("load academic query failure marker: %w", err)
	}
	if reason == "1" {
		reason = staleFallbackBusy
	}
	return reason, true, nil
}

func (s *store) markFailure(ctx context.Context, key, reason string, ttl time.Duration) error {
	if err := s.client.Set(ctx, key, reason, ttl).Err(); err != nil {
		return fmt.Errorf("save academic query failure marker: %w", err)
	}
	return nil
}

func (s *store) circuitOpen(
	ctx context.Context,
	key,
	probeToken string,
	probeTTL time.Duration,
) (open bool, retryAfter time.Duration, probeAcquired bool, err error) {
	result, err := circuitStateScript.Run(
		ctx,
		s.client,
		[]string{key},
		probeToken,
		probeTTL.Milliseconds(),
	).Slice()
	if err != nil {
		return false, 0, false, fmt.Errorf("load academic query circuit state: %w", err)
	}
	if len(result) != 3 {
		return false, 0, false, fmt.Errorf("academic query circuit state returned an invalid result")
	}
	openValue, err := redisInteger(result[0])
	if err != nil {
		return false, 0, false, err
	}
	retryMilliseconds, err := redisInteger(result[1])
	if err != nil {
		return false, 0, false, err
	}
	probe, err := redisInteger(result[2])
	if err != nil {
		return false, 0, false, err
	}
	return openValue == 1,
		time.Duration(retryMilliseconds) * time.Millisecond,
		probe == 1,
		nil
}

func (s *store) recordDeadline(
	ctx context.Context,
	key string,
	threshold int,
	window,
	openDuration time.Duration,
	completedAt time.Time,
	probeToken string,
) (open bool, retryAfter time.Duration, opened bool, err error) {
	// Keep the old helper available to package tests and out-of-tree callers.
	// Its all-deadline threshold semantics are represented as a 100% ratio.
	return s.recordDeadlineWithPolicy(
		ctx, key, threshold, threshold, 1, threshold, window,
		window, openDuration, completedAt, probeToken,
	)
}

func (s *store) recordDeadlineWithPolicy(
	ctx context.Context,
	key string,
	minimumSamples,
	minimumDeadlines int,
	ratio float64,
	hardLimit int,
	hardWindow,
	window,
	openDuration time.Duration,
	completedAt time.Time,
	probeToken string,
) (open bool, retryAfter time.Duration, opened bool, err error) {
	result, err := recordDeadlineScript.Run(
		ctx,
		s.client,
		circuitSampleKeys(key),
		minimumSamples,
		minimumDeadlines,
		int64(ratio*1000),
		hardLimit,
		hardWindow.Milliseconds(),
		window.Milliseconds(),
		openDuration.Milliseconds(),
		completedAt.UnixNano(),
		probeToken,
	).Slice()
	if err != nil {
		return false, 0, false, fmt.Errorf("record academic query deadline: %w", err)
	}
	if len(result) != 3 {
		return false, 0, false, fmt.Errorf("academic query circuit update returned an invalid result")
	}
	openValue, err := redisInteger(result[0])
	if err != nil {
		return false, 0, false, err
	}
	retryMilliseconds, err := redisInteger(result[1])
	if err != nil {
		return false, 0, false, err
	}
	openedValue, err := redisInteger(result[2])
	if err != nil {
		return false, 0, false, err
	}
	return openValue == 1,
		time.Duration(retryMilliseconds) * time.Millisecond,
		openedValue == 1,
		nil
}

func (s *store) resetCircuit(
	ctx context.Context,
	key string,
	completedAt time.Time,
	window time.Duration,
) (bool, error) {
	return s.resetCircuitWithPolicy(ctx, key, completedAt, window, false, window)
}

func (s *store) resetCircuitWithPolicy(
	ctx context.Context,
	key string,
	completedAt time.Time,
	window time.Duration,
	preserveSamples bool,
	hardWindow time.Duration,
) (bool, error) {
	return s.resetCircuitWithProbePolicy(ctx, key, completedAt, window, preserveSamples, hardWindow, "")
}

func (s *store) resetCircuitWithProbePolicy(
	ctx context.Context,
	key string,
	completedAt time.Time,
	window time.Duration,
	preserveSamples bool,
	hardWindow time.Duration,
	probeToken string,
) (bool, error) {
	preserve := "0"
	if preserveSamples {
		preserve = "1"
	}
	deleted, err := resetCircuitScript.Run(
		ctx,
		s.client,
		circuitSampleKeys(key),
		completedAt.UnixNano(),
		window.Milliseconds(),
		preserve,
		hardWindow.Milliseconds(),
		probeToken,
	).Int64()
	if err != nil {
		return false, fmt.Errorf("reset academic query circuit: %w", err)
	}
	return deleted > 0, nil
}

func (s *store) releaseCircuitProbe(ctx context.Context, key, token string) error {
	if token == "" {
		return nil
	}
	if err := releaseCircuitProbeScript.Run(ctx, s.client, []string{key}, token).Err(); err != nil {
		return fmt.Errorf("release academic query circuit probe: %w", err)
	}
	return nil
}

func (s *store) load(ctx context.Context, key string, value any) (cacheState, error) {
	state, _, err := s.loadWithMetadata(ctx, key, value)
	return state, err
}

func (s *store) loadWithMetadata(
	ctx context.Context,
	key string,
	value any,
) (cacheState, *application.CacheMetadata, error) {
	encoded, err := s.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return cacheMiss, nil, nil
	}
	if err != nil {
		return cacheMiss, nil, fmt.Errorf("load academic query cache: %w", err)
	}
	if len(encoded) > maxCachePayloadSize*2 {
		_ = s.client.Del(ctx, key).Err()
		return cacheMiss, nil, fmt.Errorf("academic query cache ciphertext exceeds safe size")
	}
	plaintext, err := s.cipher.Decrypt(encoded, key)
	if err != nil {
		_ = s.client.Del(ctx, key).Err()
		return cacheMiss, nil, fmt.Errorf("decrypt academic query cache: %w", err)
	}
	if len(plaintext) > maxCachePayloadSize*2 {
		_ = s.client.Del(ctx, key).Err()
		return cacheMiss, nil, fmt.Errorf("academic query cache record exceeds safe size")
	}
	var header struct {
		Version int `json:"version"`
	}
	if err = json.Unmarshal([]byte(plaintext), &header); err != nil {
		_ = s.client.Del(ctx, key).Err()
		return cacheMiss, nil, fmt.Errorf("decode academic query cache record")
	}
	var cachedAt, freshUntil, staleUntil int64
	var payload []byte
	switch header.Version {
	case cacheVersionV1:
		var record cacheRecordV1
		if err = decodeCacheRecord(plaintext, &record); err != nil {
			_ = s.client.Del(ctx, key).Err()
			return cacheMiss, nil, fmt.Errorf("decode academic query cache record")
		}
		freshUntil, staleUntil, payload = record.FreshUntil, record.StaleUntil, record.Payload
	case cacheVersionV2, cacheVersion:
		var record cacheRecord
		if err = decodeCacheRecord(plaintext, &record); err != nil || record.Encoding != cacheEncodingZstd {
			_ = s.client.Del(ctx, key).Err()
			return cacheMiss, nil, fmt.Errorf("decode academic query cache record")
		}
		payload, err = s.decoder.DecodeAll(record.Payload, nil)
		if err != nil || len(payload) > maxCachePayloadSize {
			_ = s.client.Del(ctx, key).Err()
			return cacheMiss, nil, fmt.Errorf("decompress academic query cache payload")
		}
		cachedAt = record.CachedAt
		freshUntil, staleUntil = record.FreshUntil, record.StaleUntil
	default:
		_ = s.client.Del(ctx, key).Err()
		return cacheMiss, nil, fmt.Errorf("decode academic query cache record")
	}
	now := time.Now().UnixMilli()
	if staleUntil <= now || freshUntil > staleUntil {
		_ = s.client.Del(ctx, key).Err()
		return cacheMiss, nil, nil
	}
	if header.Version == cacheVersion && (cachedAt <= 0 || cachedAt > freshUntil) {
		_ = s.client.Del(ctx, key).Err()
		return cacheMiss, nil, nil
	}
	if len(payload) > maxCachePayloadSize {
		return cacheMiss, nil, fmt.Errorf("academic query cache payload exceeds safe size")
	}
	if err = json.Unmarshal(payload, value); err != nil {
		_ = s.client.Del(ctx, key).Err()
		return cacheMiss, nil, fmt.Errorf("decode academic query cache payload: %w", err)
	}
	var metadata *application.CacheMetadata
	if header.Version == cacheVersion && cachedAt > 0 {
		metadata = &application.CacheMetadata{
			CachedAt: time.UnixMilli(cachedAt), FreshUntil: time.UnixMilli(freshUntil),
		}
	}
	if freshUntil > now {
		if metadata != nil {
			metadata.State = application.CacheStateFresh
		}
		return cacheFresh, metadata, nil
	}
	if metadata != nil {
		metadata.State = application.CacheStateStale
	}
	return cacheStale, metadata, nil
}

func decodeCacheRecord(plaintext string, record any) error {
	decoder := json.NewDecoder(strings.NewReader(plaintext))
	decoder.DisallowUnknownFields()
	return decoder.Decode(record)
}

func (s *store) save(
	ctx context.Context,
	key string,
	studentIndexKey string,
	value any,
	freshTTL,
	staleTTL time.Duration,
) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode academic query result: %w", err)
	}
	if len(payload) > maxCachePayloadSize {
		return fmt.Errorf("academic query result exceeds safe cache size")
	}
	now := time.Now()
	record := cacheRecord{
		Version:    cacheVersion,
		Encoding:   cacheEncodingZstd,
		CachedAt:   now.UnixMilli(),
		FreshUntil: now.Add(freshTTL).UnixMilli(),
		StaleUntil: now.Add(staleTTL).UnixMilli(),
		Payload:    s.encoder.EncodeAll(payload, nil),
	}
	plaintext, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode academic query cache record: %w", err)
	}
	encoded, err := s.cipher.Encrypt(string(plaintext), key)
	if err != nil {
		return fmt.Errorf("encrypt academic query cache: %w", err)
	}
	if _, err = saveCacheScript.Run(
		ctx,
		s.client,
		[]string{key, studentIndexKey},
		encoded,
		staleTTL.Milliseconds(),
	).Result(); err != nil {
		return fmt.Errorf("save academic query cache: %w", err)
	}
	return nil
}

func (s *store) deleteStudent(ctx context.Context, studentNo string) error {
	indexKey := s.studentIndexKey(studentNo)
	keys, err := s.client.SMembers(ctx, indexKey).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("list student academic query cache: %w", err)
	}
	_, err = s.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		if len(keys) > 0 {
			pipe.Del(ctx, keys...)
		}
		pipe.Del(ctx, indexKey)
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete student academic query cache: %w", err)
	}
	return nil
}

func (s *store) acquireLease(
	ctx context.Context,
	key string,
	ttl time.Duration,
) (string, bool, error) {
	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", false, fmt.Errorf("generate academic query lease token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	acquired, err := s.client.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return "", false, fmt.Errorf("acquire academic query lease: %w", err)
	}
	return token, acquired, nil
}

func (s *store) releaseLease(ctx context.Context, key, token string) error {
	if token == "" {
		return nil
	}
	if _, err := releaseLeaseScript.Run(ctx, s.client, []string{key}, token).Result(); err != nil {
		return fmt.Errorf("release academic query lease: %w", err)
	}
	return nil
}

func (s *store) allowGlobal(
	ctx context.Context,
	revision string,
	rate,
	burst int,
) (bool, time.Duration, error) {
	result, err := rateLimitScript.Run(
		ctx,
		s.client,
		[]string{keyPrefix + "global-rate:" + revision},
		rate,
		burst,
	).Slice()
	if err != nil {
		return false, 0, fmt.Errorf("apply academic query global rate limit: %w", err)
	}
	if len(result) != 2 {
		return false, 0, fmt.Errorf("academic query rate limiter returned an invalid result")
	}
	allowed, err := redisInteger(result[0])
	if err != nil {
		return false, 0, err
	}
	retryMilliseconds, err := redisInteger(result[1])
	if err != nil {
		return false, 0, err
	}
	return allowed == 1, time.Duration(retryMilliseconds) * time.Millisecond, nil
}

func redisInteger(value any) (int64, error) {
	switch typed := value.(type) {
	case int64:
		return typed, nil
	case string:
		var parsed int64
		if _, err := fmt.Sscan(typed, &parsed); err != nil {
			return 0, fmt.Errorf("decode academic query rate limiter result: %w", err)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("academic query rate limiter returned an invalid value")
	}
}
