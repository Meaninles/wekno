// Package main is the main package for the WeKnora server
// It contains the main function and the entry point for the server
//
// @title           WeKnora API
// @version         1.0
// @description     WeKnora 知识库管理系统 API 文档
// @termsOfService  http://swagger.io/terms/
//
// @contact.name   WeKnora Github
// @contact.url    https://github.com/Tencent/WeKnora
//
// @BasePath  /api/v1
//
// @securityDefinitions.apikey Bearer
// @in header
// @name Authorization
// @description 用户登录认证：输入 Bearer {token} 格式的 JWT 令牌

// @securityDefinitions.apikey ApiKeyAuth
// @in header
// @name X-API-Key
// @description 租户身份认证：输入 sk- 开头的 API Key
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/container"
	custombootstrap "github.com/Tencent/WeKnora/internal/custom/bootstrap"
	"github.com/Tencent/WeKnora/internal/custom/modules/dependencycontrol"
	"github.com/Tencent/WeKnora/internal/custom/modules/documentqueue"
	"github.com/Tencent/WeKnora/internal/custom/modules/runtimeprofile"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/runtime"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

func main() {
	if handled, err := runMaintenanceCommand(context.Background(), os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintf(os.Stderr, "maintenance command failed: %v\n", err)
			os.Exit(1)
		}
		return
	}
	// Set Gin mode
	if os.Getenv("GIN_MODE") == "release" {
		gin.SetMode(gin.ReleaseMode)
	} else {
		gin.SetMode(gin.DebugMode)
	}
	// Mute Gin's per-route registration spam (one line per route × ~150
	// routes) — replaced by a single summary printed after router build.
	runtime.SilenceGinRouteSpam()
	// Print the env banner before container build so operators see what
	// config landed even when DB / storage init fails.
	runtime.LogStartupEnv(context.Background())
	runtime.MarkServerStarted()

	// Build dependency injection container
	c := container.BuildContainer(runtime.GetContainer())
	profile := runtimeprofile.MustLoadFromEnv()
	if profile.Role == runtimeprofile.RoleMigration {
		err := c.Invoke(func(
			_ *custombootstrap.Handlers,
			resourceCleaner interfaces.ResourceCleaner,
		) error {
			logger.Infof(context.Background(), "migration role completed successfully")
			errs := resourceCleaner.Cleanup(context.Background())
			if len(errs) > 0 {
				return fmt.Errorf("migration cleanup failed: %v", errs)
			}
			return nil
		})
		if err != nil {
			logger.Fatalf(context.Background(), "migration role failed: %v", err)
		}
		return
	}

	// One-shot bootstrap hooks (e.g. promote env-named user to system
	// admin). Best-effort: never aborts startup — see bootstrap.go.
	runStartupBootstrap(c)

	// Run application
	err := c.Invoke(func(
		cfg *config.Config,
		router *gin.Engine,
		resourceCleaner interfaces.ResourceCleaner,
		systemSettingSvc interfaces.SystemSettingService,
		documentQueue *documentqueue.Coordinator,
		dependencyControl *dependencycontrol.Service,
		profile runtimeprofile.Profile,
	) error {
		handler := http.Handler(router)
		if !profile.ServesAPI() {
			health := gin.New()
			health.GET("/health", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{
					"status": "ok",
					"role":   profile.Role,
				})
			})
			health.GET("/ready", func(c *gin.Context) {
				if ready, reason := dependencyControl.ReadyFor(profile); !ready {
					c.JSON(http.StatusServiceUnavailable, gin.H{
						"status": "not_ready", "role": profile.Role,
						"dependency": reason,
					})
					return
				}
				c.JSON(http.StatusOK, gin.H{
					"status": "ready",
					"role":   profile.Role,
				})
			})
			// Worker metrics are served by the same lightweight listener as
			// health probes. Without this route, the role that actually owns
			// model admission and queue execution is invisible while API
			// replicas export only their process-local zero values.
			health.GET("/metrics", gin.WrapH(promhttp.Handler()))
			health.GET("/api/v1/custom/runtime-profile/status", func(c *gin.Context) {
				dependencyReady, dependencyReason := dependencyControl.ReadyFor(profile)
				c.JSON(http.StatusOK, gin.H{
					"role":              profile.Role,
					"serves_api":        profile.ServesAPI(),
					"parse_worker":      profile.RunsParseWorker(),
					"derivative_worker": profile.RunsDerivativeWorker(),
					"wiki_worker":       profile.RunsWikiWorker(),
					"maintenance":       profile.RunsMaintenance(),
					"dependency_ready":  dependencyReady,
					"dependency_reason": dependencyReason,
				})
			})
			handler = health
		}
		// Create HTTP server
		server := &http.Server{
			Handler: handler,
		}

		addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
		listener, err := listenWithRetry(addr, 10, 300*time.Millisecond)
		if err != nil {
			return fmt.Errorf("failed to start server: %v", err)
		}

		ctx, done := context.WithCancel(context.Background())

		// Start the system_settings pubsub subscriber. Runs in its own
		// goroutine and exits when ctx is cancelled at shutdown. Best-
		// effort: an error here only warns (Redis may legitimately be
		// disabled in lite-mode deployments — the service no-ops in
		// that case anyway).
		if profile.ServesAPI() {
			if err := systemSettingSvc.SubscribeRedis(ctx); err != nil {
				logger.Warnf(ctx, "[system_settings] subscribe failed: %v", err)
			}
		}

		signals := make(chan os.Signal, 1)
		signal.Notify(signals, shutdownSignals...)
		go func() {
			sig := <-signals
			logger.Infof(context.Background(), "Received signal: %v, starting server shutdown...", sig)

			// Fence root-document admission before any HTTP draining. This is
			// especially important in Kubernetes: a terminating pod must not
			// claim another complete workflow while endpoint removal and
			// graceful shutdown are propagating.
			if profile.RunsParseWorker() {
				documentQueue.MarkDraining()
			}

			// Close listener first to release port immediately,
			// so the next process can bind during our graceful drain.
			listener.Close()

			shutdownTimeout := cfg.Server.ShutdownTimeout
			if shutdownTimeout == 0 {
				shutdownTimeout = 30 * time.Second
			}
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer shutdownCancel()

			// Second signal → force close all connections immediately
			go func() {
				sig := <-signals
				logger.Warnf(context.Background(), "Received second signal: %v, forcing shutdown...", sig)
				server.Close()
			}()

			if err := server.Shutdown(shutdownCtx); err != nil {
				logger.Errorf(context.Background(), "Server forced to shutdown: %v", err)
				server.Close()
			}

			logger.Info(context.Background(), "Cleaning up resources...")
			errs := resourceCleaner.Cleanup(shutdownCtx)
			if len(errs) > 0 {
				logger.Errorf(context.Background(), "Errors occurred during resource cleanup: %v", errs)
			}
			logger.Info(context.Background(), "Server has exited")
			done()
		}()

		runtime.LogGinRouteCount(context.Background())
		logger.Infof(context.Background(), "Server is running at %s", addr)
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("server error: %v", err)
		}

		<-ctx.Done()
		return nil
	})
	if err != nil {
		logger.Fatalf(context.Background(), "Failed to run application: %v", err)
	}
}
