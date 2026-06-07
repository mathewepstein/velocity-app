package cache

import (
	"fmt"
	"strings"

	"github.com/mathewepstein/velocity/internal/config"
)

// OpenStore constructs the configured cache Store. Callers must Close it.
//
// The backend is read from the global [cache] config section: "sqlite" (the
// default and standard substrate) or "json" (the legacy month-partitioned JSON
// corpus, retained only as an opt-in fallback). A missing config file or empty
// backend resolves to sqlite, so a fresh install needs no configuration to get
// the standard substrate; a config-load failure also falls back to sqlite
// rather than blocking cache access.
func OpenStore() (Store, error) {
	backend := "sqlite"
	if cfg, err := config.Load(); err == nil {
		if b := strings.ToLower(strings.TrimSpace(cfg.Cache.Backend)); b != "" {
			backend = b
		}
	}
	switch backend {
	case "sqlite":
		return openSQLiteStore("")
	case "json":
		return JSONStore{}, nil
	default:
		return nil, fmt.Errorf("config [cache] backend = %q is not a known backend (want sqlite|json)", backend)
	}
}
