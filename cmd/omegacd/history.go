package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/obleey/omegacd/internal/store"
)

// historySettings reads OMEGACD_HISTORY_KEEP (Sync Operations per Application) and OMEGACD_HISTORY_MAX_AGE (a Go
// duration); unset, history is kept forever (Q36, Q112).
func historySettings(keep, maxAge string) (int, time.Duration, error) {
	var n int
	var age time.Duration
	var err error
	if v := strings.TrimSpace(keep); v != "" {
		if n, err = strconv.Atoi(v); err != nil || n < 1 {
			return 0, 0, fmt.Errorf("OMEGACD_HISTORY_KEEP must be a number of Sync Operations, 1 or more, not %q", keep)
		}
	}
	if v := strings.TrimSpace(maxAge); v != "" {
		if age, err = time.ParseDuration(v); err != nil || age <= 0 {
			return 0, 0, fmt.Errorf("OMEGACD_HISTORY_MAX_AGE must be a duration such as 720h, not %q", maxAge)
		}
	}
	return n, age, nil
}

// pruneHistory applies the retention now and then every interval until ctx ends, logging what went.
func pruneHistory(ctx context.Context, log *slog.Logger, st *store.Store, keep int, maxAge, every time.Duration) {
	if keep == 0 && maxAge == 0 {
		log.Debug("history kept forever: OMEGACD_HISTORY_KEEP and OMEGACD_HISTORY_MAX_AGE are unset")
		return
	}
	for {
		var olderThan time.Time
		if maxAge > 0 {
			olderThan = time.Now().Add(-maxAge)
		}
		n, err := st.PruneOps(ctx, keep, olderThan)
		switch {
		case err != nil:
			log.Error("prune history", "err", err)
		case n > 0:
			log.Info("history pruned", "deleted", n, "keep", keep, "maxAge", maxAge)
		default:
			log.Debug("history: nothing to prune", "keep", keep, "maxAge", maxAge)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}
