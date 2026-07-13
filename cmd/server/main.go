package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/compress"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/etag"
	"github.com/gofiber/fiber/v2/middleware/helmet"
	"github.com/gofiber/fiber/v2/middleware/limiter"
	fiberlogger "github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/gofiber/fiber/v2/middleware/requestid"
	"go.uber.org/zap"

	"github.com/hotelharmony/api/internal/cache"
	"github.com/hotelharmony/api/internal/config"
	"github.com/hotelharmony/api/internal/handler"
	"github.com/hotelharmony/api/internal/repository/postgres"
	"github.com/hotelharmony/api/internal/service"
	"github.com/hotelharmony/api/internal/tenant"
	"github.com/hotelharmony/api/internal/worker"
	applogger "github.com/hotelharmony/api/pkg/logger"
	"github.com/hotelharmony/api/pkg/validator"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	zlog, err := applogger.New(cfg)
	if err != nil {
		log.Fatalf("logger: %v", err)
	}
	defer func() { _ = zlog.Sync() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := postgres.New(ctx, cfg, zlog)
	if err != nil {
		zlog.Fatal("database connect failed", zap.Error(err))
	}
	defer db.Close()
	if err := db.EnsureAppSchema(ctx); err != nil {
		zlog.Fatal("database schema ensure failed", zap.Error(err))
	}

	c := cache.Cache(cache.NoopCache{})
	if cfg.Redis.URL != "" {
		redisCache, err := cache.New(ctx, cfg, zlog)
		if err != nil {
			zlog.Warn("redis unavailable; using noop cache", zap.Error(err))
		} else {
			c = redisCache
			defer func() { _ = c.Close() }()
		}
	}

	userRepo := postgres.NewUserRepository(db)
	hotelRepo := postgres.NewHotelRepository(db)
	roomRepo := postgres.NewRoomRepository(db)
	paymentRepo := postgres.NewPaymentRepository(db)
	dashboardRepo := postgres.NewDashboardRepository(db)

	tenantMgr := tenant.NewManager(db, cfg.Database.DSN, zlog)
	defer tenantMgr.Close()

	// Async worker pool — all email/SMS/AI/invoice sends run here, off the
	// request path. Wired as the process-wide default so handlers submit via
	// worker.SubmitOrRun without extra plumbing.
	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()
	pool := worker.New(8, 1024, 30*time.Second, zlog)
	pool.Start(workerCtx)
	worker.Default = pool

	authSvc := service.NewAuthService(userRepo, c, cfg)
	paymentSvc := service.NewPaymentService(roomRepo, paymentRepo, c, cfg, zlog)
	aiSvc := service.NewAIService(c, cfg, zlog)
	emailSvc := service.NewEmailService(cfg, zlog)
	smsSvc := service.NewSMSService(cfg, zlog)

	app := fiber.New(fiber.Config{
		AppName:      cfg.App.Name,
		ReadTimeout:  cfg.HTTP.ReadTimeout,
		WriteTimeout: cfg.HTTP.WriteTimeout,
		IdleTimeout:  cfg.HTTP.IdleTimeout,
		BodyLimit:    2 * 1024 * 1024,
	})
	app.Use(recover.New())
	app.Use(requestid.New())
	app.Use(helmet.New())
	app.Use(etag.New())
	app.Use(compress.New(compress.Config{Level: compress.LevelBestSpeed}))
	app.Use(limiter.New(limiter.Config{
		Max:        240,
		Expiration: time.Minute,
	}))
	app.Use(fiberlogger.New())
	app.Use(cors.New(cors.Config{
		AllowOrigins:     cfg.App.FrontendURL + ",http://localhost:8080,http://localhost:8081,http://127.0.0.1:8080,http://127.0.0.1:8081,https://client-ochre-xi-83.vercel.app,https://staff-flame.vercel.app,https://serenentra.com,https://www.serenentra.com,https://serenentra-landing.vercel.app",
		AllowHeaders:     "Origin, Content-Type, Accept, Authorization",
		AllowMethods:     "GET,POST,PUT,PATCH,DELETE,OPTIONS",
		AllowCredentials: true,
	}))

	v := validator.New()
	userHandler := handler.NewUserHandler(userRepo, authSvc, v, cfg.Auth.AccessTokenSecret)
	reservationHandler := handler.NewReservationHandler(roomRepo, cfg, emailSvc, smsSvc)
	handler.Register(app, handler.Handlers{
		Health:         handler.NewHealthHandler(db, c),
		Auth:           handler.NewAuthHandler(authSvc, v, db.Pool, c, cfg.Provisioning.TenantBaseDomain),
		Hotels:         handler.NewHotelHandler(hotelRepo, cfg),
		Payments:       handler.NewPaymentHandler(paymentSvc, cfg),
		Dashboard:      handler.NewDashboardHandler(dashboardRepo, c),
		Rooms:          handler.NewRoomHandler(roomRepo, c),
		Ops:            handler.NewOperationsHandler(db.Pool, cfg, tenantMgr, c),
		AI:             handler.NewAIHandler(aiSvc, roomRepo, dashboardRepo, cfg),
		Compat:         handler.NewCompatHandler(db.Pool, cfg),
		Users:          userHandler,
		Reservations:   reservationHandler,
		Billing:        handler.NewBillingHandler(db.Pool, emailSvc, cfg.Auth.AccessTokenSecret),
		Housekeeping:   handler.NewHousekeepingHandler(db.Pool),
		Revenue:        handler.NewRevenueHandler(db.Pool),
		Procurement:    handler.NewProcurementHandler(db.Pool),
		CRM:            handler.NewCRMHandler(db.Pool, cfg.Auth.AccessTokenSecret),
		Channel:        handler.NewChannelHandler(db.Pool),
		NightAudit:     handler.NewNightAuditHandler(db.Pool, cfg.Auth.AccessTokenSecret),
		Booking:        handler.NewBookingHandler(db.Pool),
		Asset:          handler.NewAssetHandler(db.Pool),
		Communications: handler.NewCommunicationsHandler(emailSvc, smsSvc, cfg),
		POS:            handler.NewPOSHandler(db.Pool, c, cfg.Auth.AccessTokenSecret, tenantMgr),
		Bulk:           handler.NewBulkHandler(db.Pool, cfg.Auth.AccessTokenSecret, tenantMgr, c),
		Monitoring:     handler.NewMonitoringHandler(db.Pool, c, cfg.Auth.AccessTokenSecret),
		Accounting:     handler.NewAccountingHandler(db.Pool, cfg.Auth.AccessTokenSecret),
		Demo:           handler.NewDemoHandler(db.Pool, emailSvc, cfg.Auth.AccessTokenSecret),
		Newsletter:     handler.NewNewsletterHandler(db.Pool, emailSvc, cfg.Auth.AccessTokenSecret),
	}, cfg.Auth.AccessTokenSecret, db.Pool, c, tenantMgr)

	errCh := make(chan error, 1)
	go func() {
		zlog.Info("server listening", zap.String("addr", cfg.Addr()))
		errCh <- app.Listen(cfg.Addr())
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		zlog.Fatal("server stopped", zap.Error(err))
	case sig := <-sigCh:
		zlog.Info("shutdown requested", zap.String("signal", sig.String()))
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := app.ShutdownWithContext(shutdownCtx); err != nil {
			zlog.Error("shutdown failed", zap.Error(err))
		}
		// Drain in-flight background jobs before exit (bounded).
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer drainCancel()
		if err := pool.Shutdown(drainCtx); err != nil {
			zlog.Warn("worker pool drain incomplete", zap.Error(err))
		}
	}
}
