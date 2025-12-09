package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"scriberr/internal/models"
	"scriberr/internal/transcription/interfaces"
	"syscall"
	"time"

	_ "scriberr/api-docs" // Import generated Swagger docs
	"scriberr/internal/api"
	"scriberr/internal/auth"
	"scriberr/internal/config"
	"scriberr/internal/database"
	"scriberr/internal/queue"
	"scriberr/internal/repository"
	"scriberr/internal/service"
	"scriberr/internal/transcription"
	"scriberr/internal/transcription/adapters"
	"scriberr/internal/transcription/registry"
	"scriberr/pkg/logger"

	"github.com/google/uuid"
	"github.com/modal-labs/libmodal/modal-go"
)

// Version information (set by GoReleaser)
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// @title Scriberr API
// @version 1.0
// @description Audio transcription service using WhisperX
// @termsOfService http://swagger.io/terms/

// @contact.name API Support
// @contact.url http://www.swagger.io/support
// @contact.email support@swagger.io

// @license.name MIT
// @license.url https://opensource.org/licenses/MIT

// @host localhost:8080
// @BasePath /api/v1

// @securityDefinitions.apikey ApiKeyAuth
// @in header
// @name X-API-Key

// @securityDefinitions.apikey BearerAuth
// @in header
// @name Authorization
// @description JWT token with Bearer prefix

func main() {
	// Handle version flag
	var showVersion = flag.Bool("version", false, "Show version information")
	flag.Parse()

	if *showVersion {
		fmt.Printf("Scriberr %s\n", version)
		fmt.Printf("Commit: %s\n", commit)
		fmt.Printf("Built: %s\n", date)
		os.Exit(0)
	}

	// Initialize structured logging first
	logger.Init(os.Getenv("LOG_LEVEL"))
	logger.Info("Starting Scriberr", "version", version)

	// Load configuration
	logger.Startup("config", "Loading configuration")
	cfg := config.Load()

	// Register adapters with config-based paths
	registerAdapters(cfg)

	// Initialize database
	logger.Startup("database", "Connecting to database")
	if err := database.Initialize(cfg.DatabasePath); err != nil {
		logger.Error("Failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer database.Close()

	// Initialize authentication service
	logger.Startup("auth", "Setting up authentication")
	authService := auth.NewAuthService(cfg.JWTSecret)

	// Initialize repositories
	logger.Startup("repository", "Initializing repositories")
	jobRepo := repository.NewJobRepository(database.DB)
	userRepo := repository.NewUserRepository(database.DB)
	apiKeyRepo := repository.NewAPIKeyRepository(database.DB)
	profileRepo := repository.NewProfileRepository(database.DB)
	llmConfigRepo := repository.NewLLMConfigRepository(database.DB)
	summaryRepo := repository.NewSummaryRepository(database.DB)
	chatRepo := repository.NewChatRepository(database.DB)
	noteRepo := repository.NewNoteRepository(database.DB)
	speakerMappingRepo := repository.NewSpeakerMappingRepository(database.DB)

	// Generate system API key
	_, err := createSystemAPIKey(apiKeyRepo)
	if err != nil {
		logger.Error("Failed to create System API key", "error", err)
		os.Exit(1)
	}

	// Initialize services
	logger.Startup("service", "Initializing services")
	userService := service.NewUserService(userRepo, authService)
	fileService := service.NewFileService()

	// Initialize unified transcription processor
	logger.Startup("transcription", "Initializing transcription service")
	unifiedProcessor := transcription.NewUnifiedJobProcessor(jobRepo)

	// Bootstrap embedded Python environment (for all adapters)
	logger.Startup("python", "Preparing Python environment")
	if err := unifiedProcessor.InitEmbeddedPythonEnv(); err != nil {
		logger.Error("Failed to prepare Python environment", "error", err)
		os.Exit(1)
	}

	// Initialize quick transcription service
	logger.Startup("quick-transcription", "Initializing quick transcription service")
	quickTranscriptionService, err := transcription.NewQuickTranscriptionService(cfg, unifiedProcessor)
	if err != nil {
		logger.Error("Failed to initialize quick transcription service", "error", err)
		os.Exit(1)
	}

	// Initialize task queue
	logger.Startup("queue", "Starting background processing")
	taskQueue := queue.NewTaskQueue(2, unifiedProcessor) // 2 workers
	taskQueue.Start()
	defer taskQueue.Stop()

	// Initialize API handlers
	handler := api.NewHandler(
		cfg,
		authService,
		userService,
		fileService,
		jobRepo,
		apiKeyRepo,
		profileRepo,
		userRepo,
		llmConfigRepo,
		summaryRepo,
		chatRepo,
		noteRepo,
		speakerMappingRepo,
		taskQueue,
		unifiedProcessor,
		quickTranscriptionService,
	)

	// Set up router
	router := api.SetupRoutes(handler, authService)

	// Create server
	srv := &http.Server{
		Addr:    cfg.Host + ":" + cfg.Port,
		Handler: router,
	}

	// Start server in a goroutine
	go func() {
		logger.Debug("Starting HTTP server", "host", cfg.Host, "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("Failed to start server", "error", err)
			os.Exit(1)
		}
	}()

	// Give the server a moment to start
	time.Sleep(100 * time.Millisecond)
	logger.Info("Scriberr is ready",
		"url", fmt.Sprintf("http://%s:%s", cfg.Host, cfg.Port))
	logger.Debug("API documentation available at /swagger/index.html")

	// Wait for interrupt signal to gracefully shutdown the server
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("Shutting down server")

	// Create a deadline for shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Gracefully shutdown the server
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("Server forced to shutdown", "error", err)
		os.Exit(1)
	}

	logger.Info("Server stopped")
}

func createSystemAPIKey(repo repository.APIKeyRepository) (*models.APIKey, error) {
	ctx := context.Background()
	keys, err := repo.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	var sysKey models.APIKey
	for _, key := range keys {
		if key.Name == "System" {
			return &key, nil
		}
	}

	des := "Auto generated System API key"
	sysKey = models.APIKey{
		Key:         uuid.New().String(),
		Name:        "System",
		Description: &des,
		IsActive:    true,
	}

	if err := repo.Create(ctx, &sysKey); err != nil {
		return nil, err
	}

	return &sysKey, nil
}

func registerAdapters(cfg *config.Config) {
	logger.Info("Registering adapters with environment path", "whisperx_env", cfg.WhisperXEnv)

	// Shared environment path for NVIDIA models (NeMo-based)
	nvidiaEnvPath := filepath.Join(cfg.WhisperXEnv, "parakeet")

	// Dedicated environment path for PyAnnote (to avoid dependency conflicts)
	pyannoteEnvPath := filepath.Join(cfg.WhisperXEnv, "pyannote")

	// Register transcription adapters
	whisperx := adapters.NewWhisperXAdapter(cfg.WhisperXEnv)
	mc, err := modal.NewClient()
	if err != nil {
		logger.Warn("Failed to initialize Modal client. Skipping Modal Adapter", "error", err)
	}

	registry.RegisterTranscriptionAdapter(interfaces.WhisperModal, adapters.NewModalAdapter(whisperx, mc))
	registry.RegisterTranscriptionAdapter(interfaces.WhisperRunpod, adapters.NewRunPodAdapter(whisperx))

	hasLocalWhisperX := false
	if localRunpodEndpoint := os.Getenv("LOCAL_WHISPERX_URL"); localRunpodEndpoint != "" {
		registry.RegisterTranscriptionAdapter("whisperx", adapters.NewRunPodAdapter(whisperx, adapters.WithRunpodBaseURL(localRunpodEndpoint), adapters.WithRunpodModelFamily(interfaces.LocalWhisperX)))
		hasLocalWhisperX = true
	}

	if val := os.Getenv("ENABLE_DEFAULT_ADAPTERS"); val == "" {
		return
	}

	if !hasLocalWhisperX {
		registry.RegisterTranscriptionAdapter("whisperx", whisperx)
	}
	registry.RegisterTranscriptionAdapter("parakeet",
		adapters.NewParakeetAdapter(nvidiaEnvPath))
	registry.RegisterTranscriptionAdapter("canary",
		adapters.NewCanaryAdapter(nvidiaEnvPath)) // Shares with Parakeet
	registry.RegisterTranscriptionAdapter("openai_whisper",
		adapters.NewOpenAIAdapter(cfg.OpenAIAPIKey))

	// Register diarization adapters
	registry.RegisterDiarizationAdapter("pyannote",
		adapters.NewPyAnnoteAdapter(pyannoteEnvPath)) // Dedicated environment
	registry.RegisterDiarizationAdapter("sortformer",
		adapters.NewSortformerAdapter(nvidiaEnvPath)) // Shares with Parakeet

	logger.Info("Adapter registration complete")
}
