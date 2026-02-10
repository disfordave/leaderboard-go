package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
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
	db := newPostgresDB()
	go runOutboxWorker(context.Background(), db, rdb)
	defer db.Close()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		// Check redis
		{
			ctx, cancel := context.WithTimeout(r.Context(), 200*time.Millisecond)
			defer cancel()

			_, err := rdb.Ping(ctx).Result()
			if err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{
					"status":   "not_ready",
					"redis":    "down",
					"postgres": "unknown",
				})
				return
			}
		}

		// Check postgres
		{
			ctx, cancel := context.WithTimeout(r.Context(), 200*time.Millisecond)
			defer cancel()

			if err := db.PingContext(ctx); err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{
					"status":   "not_ready",
					"redis":    "ok",
					"postgres": "down",
				})
				return
			}
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"status":   "ready",
			"redis":    "ok",
			"postgres": "ok",
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

		ctx, cancel := context.WithTimeout(r.Context(), 800*time.Millisecond)
		defer cancel()

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db begin failed"})
			return
		}
		defer tx.Rollback()

		// 1) score_events 기록(원장)
		if _, err := tx.ExecContext(ctx, `
  INSERT INTO score_events (season_id, user_id, delta)
  VALUES ($1,$2,$3)
`, seasonID, req.UserID, req.Delta); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db score_events insert failed"})
			return
		}

		// 2) outbox 기록(해야 할 일)
		payload, _ := json.Marshal(map[string]any{
			"seasonId": seasonID,
			"userId":   req.UserID,
			"delta":    req.Delta,
		})
		if _, err := tx.ExecContext(ctx, `
  INSERT INTO outbox (event_type, payload, status)
  VALUES ('score_delta', $1, 'pending')
`, payload); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db outbox insert failed"})
			return
		}

		if err := tx.Commit(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db commit failed"})
			return
		}

		// outbox 방식이면 202가 자연스러움(비동기 반영)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"seasonId": seasonID,
			"userId":   req.UserID,
			"queued":   true,
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

	// GET /v1/seasons/{sid}/leaderboard/around?userId=...&range=5
	mux.HandleFunc("GET /v1/seasons/{sid}/leaderboard/around", func(w http.ResponseWriter, r *http.Request) {
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

		rng := int64(5)
		if v := r.URL.Query().Get("range"); v != "" {
			var parsed int64
			if _, err := fmt.Sscanf(v, "%d", &parsed); err != nil || parsed < 0 || parsed > 100 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "range must be 0..100"})
				return
			}
			rng = parsed
		}

		key := fmt.Sprintf("lb:%s", seasonID)

		ctx, cancel := context.WithTimeout(r.Context(), 300*time.Millisecond)
		defer cancel()

		myRank0, err := rdb.ZRevRank(ctx, key, userID).Result()
		if err == redis.Nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "user not found in leaderboard"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "redis error"})
			return
		}

		start := myRank0 - rng
		if start < 0 {
			start = 0
		}
		end := myRank0 + rng

		zs, err := rdb.ZRevRangeWithScores(ctx, key, start, end).Result()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "redis error"})
			return
		}

		items := make([]aroundItem, 0, len(zs))
		for i, z := range zs {
			uid, ok := z.Member.(string)
			if !ok {
				uid = fmt.Sprint(z.Member)
			}
			items = append(items, aroundItem{
				Rank:   (start + int64(i)) + 1, // 1-based rank
				UserID: uid,
				Score:  z.Score,
			})
		}

		writeJSON(w, http.StatusOK, aroundResponse{
			SeasonID: seasonID,
			UserID:   userID,
			Range:    rng,
			Items:    items,
		})
	})

	// DELETE /v1/seasons/{sid}
	// DELETE /v1/seasons/{sid}
	mux.HandleFunc("DELETE /v1/seasons/{sid}", func(w http.ResponseWriter, r *http.Request) {
		sid := r.PathValue("sid")
		if sid == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing season id"})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 800*time.Millisecond)
		defer cancel()

		// Delete Redis
		key := fmt.Sprintf("lb:%s", sid)
		if err := rdb.Del(ctx, key).Err(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "redis error"})
			return
		}

		// Delete Postgres records
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db begin failed"})
			return
		}
		defer tx.Rollback()

		if _, err := tx.ExecContext(ctx,
			`DELETE FROM score_events WHERE season_id=$1`, sid); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "score_events delete failed"})
			return
		}

		if _, err := tx.ExecContext(ctx,
			`DELETE FROM outbox WHERE payload->>'seasonId'=$1`, sid); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "outbox delete failed"})
			return
		}

		if err := tx.Commit(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "db commit failed"})
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"seasonId": sid,
			"deleted":  true,
		})
	})

	fmt.Println("Leaderboard-go Server is starting on http://localhost:8080")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		fmt.Println("Error starting server:", err)
	}
}

func runOutboxWorker(ctx context.Context, db *sql.DB, rdb *redis.Client) {
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = processOneOutbox(ctx, db, rdb)
		}
	}
}

func processOneOutbox(ctx context.Context, db *sql.DB, rdb *redis.Client) error {
	c, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()

	tx, err := db.BeginTx(c, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var id int64
	var eventType string
	var payload []byte

	err = tx.QueryRowContext(c, `
		SELECT id, event_type, payload
		FROM outbox
		WHERE status='pending'
		ORDER BY id
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`).Scan(&id, &eventType, &payload)

	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(c, `
		UPDATE outbox SET status='processing', attempts=attempts+1
		WHERE id=$1
	`, id); err != nil {
		return err
	}

	var p struct {
		SeasonID string `json:"seasonId"`
		UserID   string `json:"userId"`
		Delta    int64  `json:"delta"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		_, _ = tx.ExecContext(c, `
			UPDATE outbox SET status='failed', last_error=$2
			WHERE id=$1
		`, id, "bad payload: "+err.Error())
		return tx.Commit()
	}

	if eventType == "score_delta" {
		key := fmt.Sprintf("lb:%s", p.SeasonID)
		if err := rdb.ZIncrBy(c, key, float64(p.Delta), p.UserID).Err(); err != nil {
			_, _ = tx.ExecContext(c, `
				UPDATE outbox SET status='pending', last_error=$2
				WHERE id=$1
			`, id, "redis: "+err.Error())
			return tx.Commit()
		}
	}

	if _, err := tx.ExecContext(c, `
		UPDATE outbox
		SET status='done', processed_at=now(), last_error=NULL
		WHERE id=$1
	`, id); err != nil {
		return err
	}

	return tx.Commit()
}

func newRedisClient() *redis.Client {
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	return redis.NewClient(&redis.Options{Addr: redisAddr})
}

func newPostgresDB() *sql.DB {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		dsn = "postgres://leaderboard:leaderboard@localhost:5432/leaderboard?sslmode=disable"
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		panic(err)
	}

	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(20)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		panic(err)
	}
	return db
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
