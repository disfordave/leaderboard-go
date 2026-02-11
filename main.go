package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/lib/pq"
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
	defer db.Close()
	defer rdb.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go runOutboxWorker(ctx, db, rdb)

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
					"schema":   "unknown",
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
					"schema":   "unknown",
				})
				return
			}
		}

		// Check schema
		{
			ctx, cancel := context.WithTimeout(r.Context(), 200*time.Millisecond)
			defer cancel()

			if _, err := db.ExecContext(ctx, `SELECT 1 FROM outbox LIMIT 1`); err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{
					"status":   "not_ready",
					"redis":    "ok",
					"postgres": "ok",
					"schema":   "missing",
				})
				return
			}
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"status":   "ready",
			"redis":    "ok",
			"postgres": "ok",
			"schema":   "ok",
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
		if req.Delta == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "delta must be non-zero"})
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

	srv := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		fmt.Println("Leaderboard-go Server is starting on http://localhost:8080")
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		fmt.Println("Shutdown signal received")
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			fmt.Println("Server error:", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		fmt.Println("Shutdown error:", err)
	} else {
		fmt.Println("Server stopped gracefully")
	}

}

func runOutboxWorker(ctx context.Context, db *sql.DB, rdb *redis.Client) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := processBatchOutbox(ctx, db, rdb); err != nil {
				if err != sql.ErrNoRows {
					fmt.Println("Worker error:", err)
				}
			}
		}
	}
}

func processBatchOutbox(ctx context.Context, db *sql.DB, rdb *redis.Client) error {
	const batchSize = 500

	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	tx, err := db.BeginTx(c, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(c, `
        SELECT id, event_type, payload
        FROM outbox
        WHERE status='pending'
        ORDER BY id
        FOR UPDATE SKIP LOCKED
        LIMIT $1
    `, batchSize)
	if err != nil {
		return err
	}
	defer rows.Close()

	type outboxItem struct {
		ID        int64
		EventType string
		Payload   []byte
	}
	var items []outboxItem
	for rows.Next() {
		var i outboxItem
		if err := rows.Scan(&i.ID, &i.EventType, &i.Payload); err != nil {
			return err
		}
		items = append(items, i)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if len(items) == 0 {
		return nil
	}

	ids := make([]int64, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}

	if _, err := tx.ExecContext(c, `
	UPDATE outbox
	SET status='processing', attempts=attempts+1
	WHERE id = ANY($1)
`, pq.Array(ids)); err != nil {
		return fmt.Errorf("db processing update failed: %w", err)
	}

	pipe := rdb.Pipeline()

	type cmdWithID struct {
		id  int64
		cmd *redis.FloatCmd
	}
	cmds := make([]cmdWithID, 0, len(items))

	for _, item := range items {
		var p struct {
			SeasonID string `json:"seasonId"`
			UserID   string `json:"userId"`
			Delta    int64  `json:"delta"`
		}
		if err := json.Unmarshal(item.Payload, &p); err != nil {
			_, _ = tx.ExecContext(c,
				`UPDATE outbox SET status='failed', last_error=$2 WHERE id=$1`,
				item.ID, "json error: "+err.Error(),
			)
			continue
		}

		if item.EventType != "score_delta" {
			_, _ = tx.ExecContext(c,
				`UPDATE outbox SET status='failed', last_error=$2 WHERE id=$1`,
				item.ID, "unknown event_type: "+item.EventType,
			)
			continue
		}

		key := fmt.Sprintf("lb:%s", p.SeasonID)
		cmd := pipe.ZIncrBy(c, key, float64(p.Delta), p.UserID)
		cmds = append(cmds, cmdWithID{id: item.ID, cmd: cmd})
	}

	if _, err := pipe.Exec(c); err != nil {
		return fmt.Errorf("redis pipeline failed: %w", err)
	}

	okIDs := make([]int64, 0, len(cmds))
	failIDs := make([]int64, 0)

	for _, x := range cmds {
		if x.cmd.Err() != nil {
			failIDs = append(failIDs, x.id)
		} else {
			okIDs = append(okIDs, x.id)
		}
	}

	if len(okIDs) > 0 {
		_, err := tx.ExecContext(c, `
		UPDATE outbox
		SET status='done', processed_at=now(), last_error=NULL
		WHERE id = ANY($1)
	`, pq.Array(okIDs))
		if err != nil {
			return fmt.Errorf("db bulk done update failed: %w", err)
		}
	}

	if len(failIDs) > 0 {
		_, err := tx.ExecContext(c, `
		UPDATE outbox
		SET status='pending', last_error='redis cmd error'
		WHERE id = ANY($1)
	`, pq.Array(failIDs))
		if err != nil {
			return fmt.Errorf("db bulk pending update failed: %w", err)
		}
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

	db.SetMaxOpenConns(50)
	db.SetMaxIdleConns(50)
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
