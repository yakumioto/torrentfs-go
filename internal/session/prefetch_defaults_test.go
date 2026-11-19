package session

import "github.com/yakumioto/torrentfs-go/internal/config"

// The defaults live in the configuration schema (ADR-0001); internal tests
// derive them from the same place instead of restating the numbers.
var (
	testPrefetchWindowBytes = int64(config.Default().Filesystem.Prefetch.WindowBytes)
	testPrefetchBudgetBytes = int64(config.Default().Filesystem.Prefetch.BudgetBytes)
)
