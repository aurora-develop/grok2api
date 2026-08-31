// Command grok2api is the OpenAI/Anthropic-compatible API gateway for Grok.
//
// Lifecycle mirrors the upstream Python project:
//  1. Early logging setup (env-driven, before config).
//  2. Load TOML configuration (defaults + user + env overrides).
//  3. Open the SQLite account repository and bootstrap the in-memory directory.
//  4. Build the TLS-fingerprinted transport and quota fetcher.
//  5. Reconcile the runtime selection strategy (quota vs random).
//  6. Acquire the scheduler-leader file lock — only the leader runs the
//     heavy upstream refresh loops; everyone else runs the lightweight
//     incremental directory sync.
//  7. Start the long-running goroutines:
//     - account directory sync loop (all processes),
//     - per-pool quota refresh loops (leader only),
//     - console-quota reset loop (leader only),
//     - console-expired recovery loop (leader only).
//  8. Serve HTTP until SIGINT/SIGTERM, then graceful shutdown.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aurora-develop/grok2api/internal/account"
	"github.com/aurora-develop/grok2api/internal/api"
	"github.com/aurora-develop/grok2api/internal/config"
	"github.com/aurora-develop/grok2api/internal/grok"
	"github.com/aurora-develop/grok2api/internal/logger"
	"github.com/aurora-develop/grok2api/internal/platform"
	"github.com/aurora-develop/grok2api/internal/storage"
)

// Project version (overridden at build time via -ldflags).
var Version = "dev"

func main() {
	// 1. Early logging setup — read LOG_LEVEL/LOG_FILE_ENABLED before config.
	logLevel := envOrDefault("LOG_LEVEL", "INFO")
	fileLogging := envBool("LOG_FILE_ENABLED", true)
	logDir := platform.LogDir()
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		// Fall back to stderr-only if the log directory cannot be created.
		fileLogging = false
	}
	logger.Setup(logLevel, fileLogging, logDir, 7)

	logger.Infof("application startup: service=grok2api version=%s platform=%s", Version, runtime.GOOS)

	// 2. Load configuration.
	defaultsPath := defaultsConfigPath()
	userPath := userConfigPath()
	config.SetPaths(defaultsPath, userPath)
	if err := config.Load(); err != nil {
		logger.Errorf("config load failed: error=%v", err)
		os.Exit(1)
	}
	cfg := config.Global()
	logger.Reload(
		logLevel,
		cfg.GetStr("logging.file_level", ""),
		cfg.GetInt("logging.max_files", 7),
	)

	// 3. Open the account repository (JSONL text file).
	dbPath := resolveAccountPath(cfg)
	logger.Infof("account storage configured: backend=text target=%s", dbPath)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		logger.Errorf("account storage dir create failed: error=%v", err)
		os.Exit(1)
	}
	repo := account.NewTxtRepository(dbPath)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := repo.Initialize(ctx); err != nil {
		logger.Errorf("account repository initialize failed: error=%v", err)
		os.Exit(1)
	}

	// 4. Bootstrap the in-memory account directory from the persistent store.
	directory := account.NewDirectory(repo)
	if err := directory.Bootstrap(ctx); err != nil {
		logger.Errorf("account directory bootstrap failed: error=%v", err)
		os.Exit(1)
	}
	logger.Infof("account directory bootstrapped: accounts=%d revision=%d", directory.Size(), directory.Revision())

	// 5. Build the TLS-fingerprinted transport and quota fetcher.
	transport, err := grok.NewTransport()
	if err != nil {
		logger.Errorf("transport build failed: error=%v", err)
		os.Exit(1)
	}
	usageFetcher := grok.NewUsageFetcher(transport)
	refreshSvc := account.NewRefreshService(repo, usageFetcher)

	// 6. Reconcile the runtime selection strategy from config.
	refreshEnabled := cfg.GetBool("account.refresh.enabled", false)
	_ = account.ReconcileRuntime(directory, refreshEnabled)

	// 7. Local media cache store (image/video) — rebuild indexes on startup.
	mediaStore := storage.NewLocalMediaCacheStore()
	if err := mediaStore.Rebuild(storage.MediaImage); err != nil {
		logger.Warnf("media cache image rebuild failed: error=%v", err)
	}
	if err := mediaStore.Rebuild(storage.MediaVideo); err != nil {
		logger.Warnf("media cache video rebuild failed: error=%v", err)
	}

	// 9. Build the API server and HTTP handler tree.
	server := api.NewServer(repo, directory, refreshSvc, transport, mediaStore)
	handler := server.Router()

	// 10. Start long-running goroutines.
	var wg sync.WaitGroup

	// 10a. Account directory sync loop — all processes, lightweight incremental pull.
	syncIdleInterval := envInt("ACCOUNT_SYNC_INTERVAL", 30)
	syncActiveInterval := envInt("ACCOUNT_SYNC_ACTIVE_INTERVAL", 3)
	syncIdleAfter := 5
	wg.Add(1)
	go func() {
		defer wg.Done()
		runDirectorySyncLoop(ctx, directory, syncIdleInterval, syncActiveInterval, syncIdleAfter)
	}()

	// 10b. Per-pool quota refresh loops.
	if refreshEnabled {
		pools := []string{"basic", "super", "heavy"}
		for _, pool := range pools {
			p := pool
			key := "account.refresh." + p + "_interval_sec"
			interval := cfg.GetInt(key, defaultRefreshInterval(p))
			wg.Add(1)
			go func() {
				defer wg.Done()
				runRefreshLoop(ctx, refreshSvc, p, interval)
			}()
		}
	}

	// 10c. Console-quota reset loop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		runConsoleResetLoop(ctx, refreshSvc, 30)
	}()

	// 10d. Console-expired recovery loop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		runConsoleRecoveryLoop(ctx, refreshSvc, 600)
	}()

	// 11. Start the HTTP server.
	host := envOrDefault("SERVER_HOST", "0.0.0.0")
	port := envOrDefault("SERVER_PORT", "8000")
	addr := host + ":" + port
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}

	// Listen on a separate goroutine so the main goroutine can wait for shutdown.
	serverErrCh := make(chan error, 1)
	go func() {
		logger.Infof("http server listening on %s", addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErrCh <- err
		}
		close(serverErrCh)
	}()

	// 12. Wait for SIGINT/SIGTERM or a fatal server error.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	var fatal error
	select {
	case sig := <-sigCh:
		logger.Infof("application shutdown started: signal=%s", sig.String())
	case fatal = <-serverErrCh:
		logger.Errorf("http server failed: error=%v", fatal)
	}

	// 13. Graceful shutdown: cancel context (stops goroutines), then shutdown HTTP.
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Warnf("http server shutdown timed out: error=%v", err)
	}

	wg.Wait()
	if err := repo.Close(context.Background()); err != nil {
		logger.Warnf("repository close failed: error=%v", err)
	}

	if fatal != nil {
		os.Exit(1)
	}
	logger.Infof("application shutdown completed")
}

// runDirectorySyncLoop mirrors the Python _sync_loop: aggressively poll after
// changes detected, back off to idle pace after idleAfter consecutive empty polls.
func runDirectorySyncLoop(ctx context.Context, dir *account.Directory, idleInterval, activeInterval, idleAfter int) {
	idleStreak := 0
	for {
		interval := activeInterval
		if idleStreak >= idleAfter {
			interval = idleInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(interval) * time.Second):
		}
		changed, err := dir.SyncIfChanged(ctx)
		if err != nil {
			logger.Debugf("account directory sync error: error=%v", err)
			idleStreak = idleAfter
			continue
		}
		if changed {
			idleStreak = 0
		} else {
			idleStreak++
			if idleStreak > idleAfter {
				idleStreak = idleAfter
			}
		}
	}
}

// runRefreshLoop calls refreshSvc.RefreshScheduled for the pool at the configured interval.
func runRefreshLoop(ctx context.Context, refresh *account.RefreshService, pool string, intervalSec int) {
	if intervalSec <= 0 {
		intervalSec = defaultRefreshInterval(pool)
	}
	ticker := time.NewTicker(time.Duration(intervalSec) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refreshed, failed, err := refresh.RefreshScheduled(ctx, pool)
			if err != nil {
				logger.Errorf("account refresh cycle failed: pool=%s error=%v", pool, err)
				continue
			}
			logger.Infof("account refresh cycle completed: pool=%s refreshed=%d failed=%d", pool, refreshed, failed)
		}
	}
}

// runConsoleResetLoop calls reset_expired_console_windows periodically.
func runConsoleResetLoop(ctx context.Context, refresh *account.RefreshService, intervalSec int) {
	ticker := time.NewTicker(time.Duration(intervalSec) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := refresh.ResetExpiredConsoleWindows(ctx)
			if err != nil {
				logger.Debugf("console quota reset loop error: error=%v", err)
				continue
			}
			if n > 0 {
				logger.Infof("console quota reset: reset=%d", n)
			}
		}
	}
}

// runConsoleRecoveryLoop calls recover_console_expired_accounts periodically.
func runConsoleRecoveryLoop(ctx context.Context, refresh *account.RefreshService, intervalSec int) {
	ticker := time.NewTicker(time.Duration(intervalSec) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := refresh.RecoverConsoleExpiredAccounts(ctx)
			if err != nil {
				logger.Debugf("console expired recovery loop error: error=%v", err)
				continue
			}
			if n > 0 {
				logger.Infof("console expired recovery: recovered=%d", n)
			}
		}
	}
}

// --- helpers ---

func defaultsConfigPath() string {
	if p := os.Getenv("CONFIG_DEFAULTS_PATH"); p != "" {
		return p
	}
	return filepath.Join(platform.ProjectRoot(), "config.defaults.toml")
}

func userConfigPath() string {
	if p := os.Getenv("CONFIG_PATH"); p != "" {
		return p
	}
	return platform.DataPath("config.toml")
}

func resolveAccountPath(cfg *config.Snapshot) string {
	if p := strings.TrimSpace(os.Getenv("ACCOUNT_LOCAL_PATH")); p != "" {
		return p
	}
	if p := cfg.GetStr("account.local.path", ""); p != "" {
		return p
	}
	return platform.DataPath("accounts.jsonl")
}

func defaultRefreshInterval(pool string) int {
	switch pool {
	case "basic":
		return 86400
	case "super", "heavy":
		return 7200
	}
	return 7200
}

func envOrDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return def
	}
	return n
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}
