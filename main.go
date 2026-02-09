package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
)

type scoreUpdateRequest struct {
	UserID string `json:"userId"`
	Delta  int64  `json:"delta"`
}

type scoreUpdateResponse struct {
	SeasonID string  `json:"seasonId"`
	UserID   string  `json:"userId"`
	Score    float64 `json:"score"`
}

type leaderboardItem struct {
	UserID string  `json:"userId"`
	Score  float64 `json:"score"`
}

type topResponse struct {
	SeasonID string            `json:"seasonId"`
	Items    []leaderboardItem `json:"items"`
}

type rankResponse struct {
	SeasonID string  `json:"seasonId"`
	UserID   string  `json:"userId"`
	Rank     int64   `json:"rank"` // 1-based
	Score    float64 `json:"score"`
}

type aroundItem struct {
	Rank   int64   `json:"rank"` // 1-based
	UserID string  `json:"userId"`
	Score  float64 `json:"score"`
}

type aroundResponse struct {
	SeasonID string       `json:"seasonId"`
	UserID   string       `json:"userId"`
	Range    int64        `json:"range"`
	Items    []aroundItem `json:"items"`
}

func main() {
	rdb := newRedisClient()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 200*time.Millisecond)
		defer cancel()

		_, err := rdb.Ping(ctx).Result()
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"status": "not_ready",
				"redis":  "down",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "ready",
			"redis":  "ok",
		})
	})

	// POST /v1/seasons/{sid}/scores
	mux.HandleFunc("POST /v1/seasons/{sid}/scores", func(w http.ResponseWriter, r *http.Request) {
		seasonID := r.PathValue("sid")
		if seasonID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing season id"})
			return
		}

		const maxBodyBytes = 1 << 20 // 1 MB
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var req scoreUpdateRequest
		if err := dec.Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
			return
		}
		if req.UserID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "userId is required"})
			return
		}

		// Key: "lb:{seasonID}"
		key := fmt.Sprintf("lb:%s", seasonID)

		ctx, cancel := context.WithTimeout(r.Context(), 300*time.Millisecond)
		defer cancel()

		score, err := rdb.ZIncrBy(ctx, key, float64(req.Delta), req.UserID).Result()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "redis error"})
			return
		}

		writeJSON(w, http.StatusOK, scoreUpdateResponse{
			SeasonID: seasonID,
			UserID:   req.UserID,
			Score:    score,
		})
	})

	// GET /v1/seasons/{sid}/leaderboard/top?limit=10
	mux.HandleFunc("GET /v1/seasons/{sid}/leaderboard/top", func(w http.ResponseWriter, r *http.Request) {
		seasonID := r.PathValue("sid")
		if seasonID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing season id"})
			return
		}

		limit := 10
		if v := r.URL.Query().Get("limit"); v != "" {
			var parsed int
			if _, err := fmt.Sscanf(v, "%d", &parsed); err != nil || parsed <= 0 || parsed > 1000 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "limit must be 1..1000"})
				return
			}
			limit = parsed
		}

		key := fmt.Sprintf("lb:%s", seasonID)

		ctx, cancel := context.WithTimeout(r.Context(), 300*time.Millisecond)
		defer cancel()

		// WITHSCORES=true
		zs, err := rdb.ZRevRangeWithScores(ctx, key, 0, int64(limit-1)).Result()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "redis error"})
			return
		}

		items := make([]leaderboardItem, 0, len(zs))
		for _, z := range zs {
			uid, ok := z.Member.(string)
			if !ok {
				uid = fmt.Sprint(z.Member)
			}
			items = append(items, leaderboardItem{
				UserID: uid,
				Score:  z.Score,
			})
		}

		writeJSON(w, http.StatusOK, topResponse{
			SeasonID: seasonID,
			Items:    items,
		})
	})

	// GET /v1/seasons/{sid}/leaderboard/rank?userId=...
	mux.HandleFunc("GET /v1/seasons/{sid}/leaderboard/rank", func(w http.ResponseWriter, r *http.Request) {
		seasonID := r.PathValue("sid")
		if seasonID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing season id"})
			return
		}

		userID := r.URL.Query().Get("userId")
		if userID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "userId is required"})
			return
		}

		key := fmt.Sprintf("lb:%s", seasonID)

		ctx, cancel := context.WithTimeout(r.Context(), 300*time.Millisecond)
		defer cancel()

		rank0, err := rdb.ZRevRank(ctx, key, userID).Result()
		if err == redis.Nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "user not found in leaderboard"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "redis error"})
			return
		}

		score, err := rdb.ZScore(ctx, key, userID).Result()
		if err == redis.Nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "user not found in leaderboard"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "redis error"})
			return
		}

		writeJSON(w, http.StatusOK, rankResponse{
			SeasonID: seasonID,
			UserID:   userID,
			Rank:     rank0 + 1,
			Score:    score,
		})
	})

	// DELETE /v1/seasons/{sid}
	mux.HandleFunc("DELETE /v1/seasons/{sid}", func(w http.ResponseWriter, r *http.Request) {
		sid := r.PathValue("sid")
		if sid == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing season id"})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 300*time.Millisecond)
		defer cancel()

		key := fmt.Sprintf("lb:%s", sid)
		if err := rdb.Del(ctx, key).Err(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "redis error"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"seasonId": sid, "deleted": true})
	})

	fmt.Println("Leaderboard-go Server is starting on http://localhost:8080")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		fmt.Println("Error starting server:", err)
	}
}

func newRedisClient() *redis.Client {
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	return redis.NewClient(&redis.Options{Addr: redisAddr})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
