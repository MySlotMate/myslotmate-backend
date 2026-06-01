package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"

	"myslotmate-backend/internal/config"
	"myslotmate-backend/internal/controller"
	"myslotmate-backend/internal/db"
	cfgfirebase "myslotmate-backend/internal/firebase"
	"myslotmate-backend/internal/lib/event"
	"myslotmate-backend/internal/lib/identity"
	"myslotmate-backend/internal/lib/notification"
	"myslotmate-backend/internal/lib/payment"
	"myslotmate-backend/internal/lib/payout"
	"myslotmate-backend/internal/lib/rag"
	"myslotmate-backend/internal/lib/realtime"
	"myslotmate-backend/internal/lib/storage"
	"myslotmate-backend/internal/lib/worker"
	"myslotmate-backend/internal/models"
	"myslotmate-backend/internal/repository"
	"myslotmate-backend/internal/server"
	"myslotmate-backend/internal/service"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Singleton Pattern: Database Connection
	dbConn, err := db.Open(cfg.Database.URL)
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer dbConn.Close()

	if err := dbConn.PingContext(ctx); err != nil {
		log.Printf("Warning: failed to ping database (is it running?): %v", err)
	}

	// Observer Pattern: Event Dispatcher (Singleton)
	dispatcher := event.GetDispatcher()

	socketService, err := realtime.NewSocketService()
	if err != nil {
		log.Printf("Warning: failed to initialize socket service: %v", err)
	} else {
		socketService.Start()
		defer socketService.Close()
	}

	// Executor Pattern: Worker Pool
	workerPool := worker.NewWorkerPool(5, 100)
	workerPool.Start()
	defer workerPool.Stop(context.Background())

	fbApp, err := cfgfirebase.NewApp(ctx, cfgfirebase.Config{
		CredentialsFile: cfg.Firebase.CredentialsFile,
		ProjectID:       cfg.Firebase.ProjectID,
	})
	if err != nil {
		log.Fatalf("failed to initialize firebase app: %v", err)
	}

	// Upload service backed by AWS S3 (nil-safe if bucket not configured)
	var uploadService *storage.UploadService
	if cfg.S3.Bucket != "" && cfg.S3.AccessKey != "" && cfg.S3.SecretKey != "" {
		awsCfg, err := awscfg.LoadDefaultConfig(ctx,
			awscfg.WithRegion(cfg.S3.Region),
			awscfg.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
				cfg.S3.AccessKey, cfg.S3.SecretKey, "",
			)),
		)
		if err != nil {
			log.Fatalf("failed to load AWS config: %v", err)
		}
		s3Client := s3.NewFromConfig(awsCfg)
		uploadService = storage.NewUploadService(s3Client, cfg.S3.Bucket, cfg.S3.Region)
		log.Printf("✓ AWS S3 upload service enabled: bucket=%s, region=%s", cfg.S3.Bucket, cfg.S3.Region)
	} else {
		log.Println("✗ WARNING: AWS S3 not fully configured — file uploads DISABLED")
		log.Println("  Set these environment variables to enable uploads:")
		log.Println("    AWS_S3_BUCKET=your-bucket-name")
		log.Println("    AWS_S3_REGION=ap-south-1")
		log.Println("    AWS_ACCESS_KEY_ID=your-access-key")
		log.Println("    AWS_SECRET_ACCESS_KEY=your-secret-key")
	}

	// Repository Pattern: Data Layer
	userRepo := repository.NewUserRepository(dbConn)
	hostRepo := repository.NewHostRepository(dbConn)
	eventRepo := repository.NewEventRepository(dbConn)
	bookingRepo := repository.NewBookingRepository(dbConn)
	reviewRepo := repository.NewReviewRepository(dbConn)
	inboxRepo := repository.NewInboxRepository(dbConn)
	accountRepo := repository.NewAccountRepository(dbConn)
	paymentRepo := repository.NewPaymentRepository(dbConn)
	payoutRepo := repository.NewPayoutRepository(dbConn)
	supportRepo := repository.NewSupportRepository(dbConn)
	savedExpRepo := repository.NewSavedExperienceRepository(dbConn)
	ledgerRepo := repository.NewTransactionLedgerRepository(dbConn)
	blogRepo := repository.NewBlogRepository(dbConn)
	experienceTemplateRepo := repository.NewExperienceTemplateRepository(dbConn)

	// Ensure platform account exists for fee tracking
	if err := ensurePlatformAccount(ctx, accountRepo); err != nil {
		log.Printf("Warning: failed to ensure platform account exists: %v", err)
	}

	// Strategy Pattern: Identity Provider
	aadharProvider := identity.NewSetuAadharProvider(identity.SetuConfig{
		BaseURL:           cfg.Setu.BaseURL,
		ClientID:          cfg.Setu.ClientID,
		ClientSecret:      cfg.Setu.ClientSecret,
		ProductInstanceID: cfg.Setu.ProductInstanceID,
	})
	log.Println("Using Setu Aadhar Provider")

	// Strategy Pattern: Payout Provider (Cashfree)
	cfClientID := cfg.Cashfree.ClientID
	cfClientSecret := cfg.Cashfree.ClientSecret
	if cfClientID == "" || cfClientSecret == "" {
		log.Println("Warning: Cashfree credentials not configured — payouts disabled (using dummy keys)")
		cfClientID = "cf_dummy_client_id"
		cfClientSecret = "cf_dummy_client_secret"
	}
	payoutProvider := payout.NewCashfreeProvider(payout.CashfreeConfig{
		BaseURL:       cfg.Cashfree.BaseURL,
		ClientID:      cfClientID,
		ClientSecret:  cfClientSecret,
		PublicKey:     cfg.Cashfree.PublicKey,
		WebhookSecret: cfg.Cashfree.WebhookSecret,
		APIVersion:    cfg.Cashfree.APIVersion,
	})
	if cfg.Cashfree.PublicKey == "" {
		log.Println("Warning: Cashfree public key is empty!")
	} else {
		log.Printf("[API] Cashfree provider initialized with public key (length=%d)\n", len(cfg.Cashfree.PublicKey))
	}
	log.Println("Using Cashfree Payout Provider")

	// Strategy Pattern: Payment Collection Provider (Razorpay Standard)
	rzpKeyID := cfg.Razorpay.KeyID
	rzpKeySecret := cfg.Razorpay.KeySecret
	if rzpKeyID == "" || rzpKeySecret == "" {
		log.Println("Warning: Razorpay credentials not configured — payment collection disabled (using dummy keys)")
		rzpKeyID = "rzp_test_dummy"
		rzpKeySecret = "dummy_secret"
	}
	paymentWebhookSecret := cfg.Razorpay.PaymentWebhookSecret
	if paymentWebhookSecret == "" {
		paymentWebhookSecret = cfg.Razorpay.WebhookSecret // fallback to shared secret
	}
	paymentProvider := payment.NewRazorpayProvider(payment.RazorpayConfig{
		KeyID:         rzpKeyID,
		KeySecret:     rzpKeySecret,
		WebhookSecret: paymentWebhookSecret,
	})
	log.Println("Using Razorpay Payment Collection Provider")

	// Initialize notification service
	notifService, err := initializeNotificationService(&cfg.Twilio, &cfg.SMTP, dbConn)
	if err != nil {
		log.Printf("Warning: failed to initialize notification service: %v", err)
	}

	userService := service.NewUserService(userRepo, hostRepo, savedExpRepo, accountRepo, paymentRepo, ledgerRepo, workerPool, dispatcher, aadharProvider, paymentProvider, notifService)
	hostService := service.NewHostService(hostRepo, userRepo, eventRepo, bookingRepo, reviewRepo, payoutRepo, accountRepo, dispatcher)
	bookingService := service.NewBookingService(dbConn, bookingRepo, eventRepo, accountRepo, paymentRepo, payoutRepo, hostRepo, userRepo, ledgerRepo, userService, dispatcher, notifService)
	eventService := service.NewEventService(eventRepo, bookingRepo, accountRepo, ledgerRepo, dispatcher, bookingService)
	reviewService := service.NewReviewService(reviewRepo, eventRepo, hostRepo, dispatcher)
	inboxService := service.NewInboxService(inboxRepo, eventRepo, socketService)
	supportService := service.NewSupportService(supportRepo)
	payoutService := service.NewPayoutService(payoutRepo, accountRepo, paymentRepo, bookingRepo, hostRepo, ledgerRepo, payoutProvider, dispatcher)

	// Initialize reminder scheduler
	var reminderScheduler *notification.ReminderScheduler
	if notifService != nil {
		reminderScheduler = initializeReminderScheduler(dbConn, notifService, 5, 90)
		reminderScheduler.Start(5 * time.Minute)
	}

	userController := controller.NewUserController(userService)
	hostController := controller.NewHostController(hostService)
	eventController := controller.NewEventController(eventService)
	bookingController := controller.NewBookingController(bookingService)
	reviewController := controller.NewReviewController(reviewService)
	inboxController := controller.NewInboxController(inboxService)
	payoutController := controller.NewPayoutController(payoutService, userRepo, hostRepo, fbApp.Auth)
	webhookController := controller.NewWebhookController(payoutService, userService, payoutProvider, paymentProvider)
	supportController := controller.NewSupportController(supportService, uploadService)
	uploadController := controller.NewUploadController(uploadService)
	adminController := controller.NewAdminController(hostService, payoutService, userService, fbApp.Auth, cfg.AdminEmail)
	blogController := controller.NewBlogController(blogRepo, userRepo, fbApp.Auth, cfg.AdminEmail)
	experienceTemplateController := controller.NewExperienceTemplateController(experienceTemplateRepo)

	// Initialize RAG components
	ragEngine := rag.NewRAGEngine(
		dbConn,
		cfg.Gemini.APIKey,
		cfg.Gemini.EmbeddingDimension,
		cfg.Pinecone.APIKey,
		cfg.Pinecone.Host,
		cfg.Pinecone.IndexName,
		10, // topK results
		20, // maxMemory messages
	)
	ragChatCtrl := controller.NewRAGChatController(ragEngine)

	// Initialize RAG Document components
	docStore := rag.NewDocumentStore(dbConn)
	ragDocCtrl := controller.NewRAGDocumentController(docStore)

	router := server.NewRouter(
		fbApp,
		socketService,
		userController,
		hostController,
		eventController,
		bookingController,
		reviewController,
		inboxController,
		payoutController,
		webhookController,
		supportController,
		uploadController,
		adminController,
		blogController,
		experienceTemplateController,
		ragChatCtrl,
		ragDocCtrl,
	)

	srv := &http.Server{
		Addr:         ":" + cfg.HTTPPort,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server in background.
	go func() {
		log.Printf("HTTP server listening on :%s", cfg.HTTPPort)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	// Self-ping keep-alive: prevents Render free-tier from spinning down.
	if cfg.RenderExternalURL != "" {
		go keepAlive(ctx, cfg.RenderExternalURL+"/health")
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down server...")

	// Stop reminder scheduler
	if reminderScheduler != nil {
		reminderScheduler.Stop()
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	} else {
		log.Println("server stopped cleanly")
	}
}

// keepAlive pings the given URL every 10 minutes to prevent
// Render free-tier instances from spinning down due to inactivity.
func keepAlive(ctx context.Context, url string) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	client := &http.Client{Timeout: 15 * time.Second}
	log.Printf("Keep-alive enabled: pinging %s every 10m", url)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			resp, err := client.Get(url)
			if err != nil {
				log.Printf("keep-alive ping failed: %v", err)
				continue
			}
			resp.Body.Close()
			log.Printf("keep-alive ping: %s", resp.Status)
		}
	}
}

// ensurePlatformAccount creates the platform account if it doesn't exist.
// This account tracks all platform fees collected from bookings.
func ensurePlatformAccount(ctx context.Context, accountRepo repository.AccountRepository) error {
	// Check if platform account exists
	platformAccount, err := accountRepo.GetByOwner(ctx, models.AccountOwnerPlatform, uuid.Nil)
	if err != nil {
		return err
	}

	// If it already exists, we're done
	if platformAccount != nil {
		log.Println("Platform account already exists")
		return nil
	}

	// Create platform account
	account := &models.Account{
		ID:           uuid.New(),
		OwnerType:    models.AccountOwnerPlatform,
		OwnerID:      uuid.Nil,
		BalanceCents: 0,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}

	if err := accountRepo.Create(ctx, account); err != nil {
		return err
	}

	log.Printf("✓ Platform account created: %s", account.ID)
	return nil
}

// initializeNotificationService creates and returns a fully configured notification service
func initializeNotificationService(
	twilioConfig *config.TwilioConfig,
	smtpConfig *config.SMTPConfig,
	db *sql.DB,
) (notification.NotificationService, error) {
	if twilioConfig.AccountSID == "" || twilioConfig.AuthToken == "" {
		log.Println("Warning: Twilio credentials not configured — notifications disabled")
		return nil, nil
	}

	// Create repositories
	bookingRepo := repository.NewBookingRepository(db)
	eventRepo := repository.NewEventRepository(db)
	userRepo := repository.NewUserRepository(db)

	// Create and return the Twilio notification service
	svc := notification.NewTwilioNotificationService(
		twilioConfig,
		smtpConfig,
		bookingRepo,
		eventRepo,
		userRepo,
	)

	log.Println("✓ Notification service initialized (Twilio with Email)")
	return svc, nil
}

// initializeReminderScheduler creates and returns a configured reminder scheduler
func initializeReminderScheduler(
	db *sql.DB,
	notifService notification.NotificationService,
	checkIntervalMins int,
	reminderMinsBefore int,
) *notification.ReminderScheduler {
	// Create repositories
	bookingRepo := repository.NewBookingRepository(db)
	eventRepo := repository.NewEventRepository(db)
	userRepo := repository.NewUserRepository(db)

	// Create scheduler
	scheduler := notification.NewReminderScheduler(
		bookingRepo,
		eventRepo,
		userRepo,
		notifService,
		reminderMinsBefore,
	)

	log.Printf("✓ Reminder scheduler initialized (check every %d min, remind %d min before)", checkIntervalMins, reminderMinsBefore)
	return scheduler
}
