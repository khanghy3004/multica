package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// usageRollupInterval is how often we invoke
	// rollup_task_usage_hourly() to advance the watermark and refresh
	// the hourly aggregate. Mirrors the cadence suggested by the
	// migration 102 comment block ("*/5 * * * *") which assumes a
	// pg_cron-driven schedule. Embedding it as a Go goroutine removes
	// the pg_cron dependency for self-hosted installs that don't have
	// the extension available.
	usageRollupInterval = 5 * time.Minute
	// usageRollupQueryTimeout caps a single tick so a slow rollup
	// cannot stall subsequent ticks. The 1-day cap inside the SQL
	// function bounds the work per call, so 2 minutes is a comfortable
	// upper bound even on a backlog catch-up.
	usageRollupQueryTimeout = 2 * time.Minute
)

// runUsageRollupLoop periodically advances task_usage_hourly by calling
// the rollup function in the database. Idempotent: the function uses an
// advisory lock and a watermark so concurrent invocations short-circuit
// to a no-op.
//
// First tick fires immediately on startup so a fresh self-hosted deploy
// doesn't show "0 tokens" for the first 5 minutes after the first task
// completes. Subsequent ticks honour the interval.
func runUsageRollupLoop(ctx context.Context, pool *pgxpool.Pool) {
	runUsageRollupOnce(ctx, pool)

	ticker := time.NewTicker(usageRollupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runUsageRollupOnce(ctx, pool)
		}
	}
}

func runUsageRollupOnce(ctx context.Context, pool *pgxpool.Pool) {
	tickCtx, cancel := context.WithTimeout(ctx, usageRollupQueryTimeout)
	defer cancel()

	var rows int64
	err := pool.QueryRow(tickCtx, "SELECT rollup_task_usage_hourly()").Scan(&rows)
	if err != nil {
		// Don't escalate — the rollup is best-effort and the next tick
		// will retry. A noisy log would drown the legitimate "no work"
		// case (rows == 0), so log at Warn only on real errors.
		if ctx.Err() == nil {
			slog.Warn("usage rollup tick failed", "error", err)
		}
		return
	}
	if rows > 0 {
		slog.Info("usage rollup tick", "rows_touched", rows)
	}
}
