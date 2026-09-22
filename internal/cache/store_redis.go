package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore uses Redis with sorted sets and hash storage for vector search.
// Uses brute-force cosine similarity over stored embeddings (for Redis without
// RediSearch/vector module). For production at scale, consider Redis Stack with
// FT.CREATE ... VECTOR.
type RedisStore struct {
	client *redis.Client
	prefix string
}

type redisIndexedEntry struct {
	id       string
	indexKey string
	created  float64
}

// NewRedisStore creates a Redis-backed cache store.
func NewRedisStore(redisURL, prefix string) (*RedisStore, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("redis: parse URL: %w", err)
	}
	client := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis: ping: %w", err)
	}
	if prefix == "" {
		prefix = "hivecache:"
	}
	return &RedisStore{client: client, prefix: prefix}, nil
}

func (s *RedisStore) Search(ctx context.Context, teamID, model string, queryEmbedding []float32, topK int) ([]Entry, error) {
	// Get all entry IDs for this team+model
	key := s.indexKey(teamID, model)
	ids, err := s.client.ZRangeArgs(ctx, redis.ZRangeArgs{
		Key:   key,
		Start: 0,
		Stop:  -1,
		Rev:   true,
	}).Result()
	if err != nil {
		return nil, err
	}

	type scored struct {
		entry Entry
		score float64
	}
	var results []scored

	for _, id := range ids {
		data, err := s.client.Get(ctx, s.entryKey(id)).Bytes()
		if err != nil {
			continue
		}
		var e Entry
		if err := json.Unmarshal(data, &e); err != nil {
			continue
		}
		sim := CosineSimilarity(queryEmbedding, e.QueryEmbedding)
		results = append(results, scored{entry: e, score: sim})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})
	if len(results) > topK {
		results = results[:topK]
	}

	out := make([]Entry, len(results))
	for i, r := range results {
		out[i] = r.entry
	}
	return out, nil
}

func (s *RedisStore) Insert(ctx context.Context, entry Entry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}

	pipe := s.client.Pipeline()
	pipe.Set(ctx, s.entryKey(entry.ID), data, 0)
	pipe.ZAdd(ctx, s.indexKey(entry.TeamID, entry.Model), redis.Z{
		Score:  float64(entry.CreatedAt.Unix()),
		Member: entry.ID,
	})
	_, err = pipe.Exec(ctx)
	return err
}

func (s *RedisStore) Delete(ctx context.Context, id string) error {
	// Get entry to find its index key
	data, err := s.client.Get(ctx, s.entryKey(id)).Bytes()
	if err != nil {
		return err
	}
	var e Entry
	if err := json.Unmarshal(data, &e); err != nil {
		return err
	}

	pipe := s.client.Pipeline()
	pipe.Del(ctx, s.entryKey(id))
	pipe.ZRem(ctx, s.indexKey(e.TeamID, e.Model), id)
	_, err = pipe.Exec(ctx)
	return err
}

func (s *RedisStore) Evict(ctx context.Context, maxAge time.Duration, maxEntries int) error {
	cutoff := float64(time.Now().Add(-maxAge).Unix())
	expired, kept, err := s.scanEvictionCandidates(ctx, cutoff)
	if err != nil {
		return err
	}

	// maxEntries is a global cache cap, matching the memory and pgvector
	// backends rather than applying the limit independently per tenant/model.
	expired = append(expired, oldestExcessRedisEntries(kept, maxEntries)...)
	return s.deleteIndexedEntries(ctx, expired)
}

func (s *RedisStore) scanEvictionCandidates(ctx context.Context, cutoff float64) ([]redisIndexedEntry, []redisIndexedEntry, error) {
	var expired []redisIndexedEntry
	var kept []redisIndexedEntry
	iter := s.client.Scan(ctx, 0, s.prefix+"idx:*", 100).Iterator()
	for iter.Next(ctx) {
		indexKey := iter.Val()
		members, err := s.client.ZRangeWithScores(ctx, indexKey, 0, -1).Result()
		if err != nil {
			return nil, nil, err
		}
		for _, member := range members {
			id, ok := member.Member.(string)
			if !ok {
				continue
			}
			candidate := redisIndexedEntry{id: id, indexKey: indexKey, created: member.Score}
			if member.Score <= cutoff {
				expired = append(expired, candidate)
			} else {
				kept = append(kept, candidate)
			}
		}
	}
	if err := iter.Err(); err != nil {
		return nil, nil, err
	}
	return expired, kept, nil
}

func oldestExcessRedisEntries(kept []redisIndexedEntry, maxEntries int) []redisIndexedEntry {
	if maxEntries >= 0 && len(kept) > maxEntries {
		sort.Slice(kept, func(i, j int) bool {
			return kept[i].created < kept[j].created
		})
		excess := len(kept) - maxEntries
		return kept[:excess]
	}
	return nil
}

func (s *RedisStore) deleteIndexedEntries(ctx context.Context, entries []redisIndexedEntry) error {
	if len(entries) == 0 {
		return nil
	}
	pipe := s.client.Pipeline()
	for _, candidate := range entries {
		pipe.Del(ctx, s.entryKey(candidate.id))
		pipe.ZRem(ctx, candidate.indexKey, candidate.id)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (s *RedisStore) Count(ctx context.Context) (int, error) {
	var total int
	iter := s.client.Scan(ctx, 0, s.prefix+"idx:*", 100).Iterator()
	for iter.Next(ctx) {
		n, err := s.client.ZCard(ctx, iter.Val()).Result()
		if err != nil {
			return 0, err
		}
		total += int(n)
	}
	if err := iter.Err(); err != nil {
		return 0, err
	}
	return total, nil
}

func (s *RedisStore) Flush(ctx context.Context) error {
	pipe := s.client.Pipeline()
	iter := s.client.Scan(ctx, 0, s.prefix+"*", 100).Iterator()
	for iter.Next(ctx) {
		pipe.Del(ctx, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return err
	}
	if len(pipe.Cmds()) == 0 {
		return nil
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (s *RedisStore) Close() error {
	return s.client.Close()
}

func (s *RedisStore) indexKey(teamID, model string) string {
	return s.prefix + "idx:" + teamID + ":" + model
}

func (s *RedisStore) entryKey(id string) string {
	return s.prefix + "entry:" + id
}
