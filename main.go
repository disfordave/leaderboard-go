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
	UserID string  `json:"userId"`
	Delta  float64 `json:"delta"`
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

func main() {
	rdb := newRedisClient()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Hello, Go!")
	})

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

		var req scoreUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
			return
		}
		if req.UserID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "userId is required"})
			return
		}

		// Key: "go:{seasonID}"
		key := fmt.Sprintf("go:%s", seasonID)

		ctx, cancel := context.WithTimeout(r.Context(), 300*time.Millisecond)
		defer cancel()

		score, err := rdb.ZIncrBy(ctx, key, req.Delta, req.UserID).Result()
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
