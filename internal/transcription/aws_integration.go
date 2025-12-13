package transcription

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"scriberr/internal/database"
	"scriberr/internal/models"
	"scriberr/internal/repository"
	"scriberr/internal/transcription/interfaces"
	"scriberr/pkg/logger"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebTypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const (
	DefaultEventBridgeSource = "scriberr.transcribe"
)

// S3JobProcessor implements the existing JobProcessor interface using the new unified service
type S3JobProcessor struct {
	unifiedProcessor  *UnifiedJobProcessor
	jobRepo           repository.JobRepository
	uploadDir         string
	s3Client          *s3.Client
	eventBridgeClient *eventbridge.Client
}

// NewS3JobProcessor creates a new job processor using the unified service
func NewS3JobProcessor(unifiedProcessor *UnifiedJobProcessor, jobRepo repository.JobRepository, uploadDir string) (*S3JobProcessor, error) {
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}

	client := s3.NewFromConfig(cfg)
	eventBridgeClient := eventbridge.NewFromConfig(cfg)

	return &S3JobProcessor{
		s3Client:          client,
		eventBridgeClient: eventBridgeClient,
		uploadDir:         uploadDir,
		unifiedProcessor:  unifiedProcessor,
		jobRepo:           jobRepo,
	}, nil
}

// Initialize prepares the job processor
func (u *S3JobProcessor) Initialize(ctx context.Context) error {
	return u.unifiedProcessor.Initialize(ctx)
}

// ProcessJob implements JobProcessor interface
func (u *S3JobProcessor) ProcessJob(ctx context.Context, jobID string) error {
	err := u.ProcessSingleJob(ctx, jobID)
	event := "COMPLETED"
	if err != nil {
		event = "FAILED"
	}

	go u.publishNotifications(jobID, event)

	return err
}

func (u *S3JobProcessor) ProcessSingleJob(ctx context.Context, jobID string) error {
	job, err := u.jobRepo.FindByID(ctx, jobID)
	if err != nil {
		return err
	}

	if job.Status == models.StatusPending {
		job.Status = models.StatusProcessing
		if err = u.jobRepo.Update(ctx, job); err != nil {
			return fmt.Errorf("failed to update job status to STATUS_PROCESSING: %w", err)
		}
	}

	filename := filepath.Base(job.AudioPath)

	isS3Job := false
	if job.AudioUri != nil && strings.HasPrefix(*job.AudioUri, "s3://") {
		isS3Job = true
		filename = filepath.Base(*job.AudioUri)
		audioPath := filepath.Join(u.uploadDir, filename)
		err := u.downloadS3File(ctx, *job.AudioUri, audioPath)
		if err != nil {
			return err
		}
		job.AudioPath = audioPath
		if err = u.jobRepo.Update(ctx, job); err != nil {
			return err
		}
	}

	if err = u.unifiedProcessor.ProcessJob(ctx, jobID); err != nil {
		return err
	}

	if !isS3Job {
		return nil
	}

	//_ = os.Remove(job.AudioPath)
	// Load the processed result back
	var processedJob models.TranscriptionJob
	if loadErr := database.DB.Where("id = ?", jobID).First(&processedJob).Error; loadErr != nil {
		return loadErr
	}

	transcript := ""
	if processedJob.Transcript != nil {
		transcript = *processedJob.Transcript
	}

	if transcript == "" || processedJob.OutputBucketName == nil {
		logger.Debug("Transcript empty or OutputBucketName not set, skipping S3 upload", "job_id", jobID)
		return nil
	}

	outputBucket := processedJob.OutputBucketName
	transcriptFilename := fmt.Sprintf("%s.json", getJobName(processedJob))

	var tags []types.Tag
	if processedJob.Tags != nil {
		if err := json.Unmarshal([]byte(*processedJob.Tags), &tags); err != nil {
			return fmt.Errorf("failed to parse job tags: %w", err)
		}
	} else {
		tags = make([]types.Tag, 0)
	}

	tags = append(tags, types.Tag{Key: aws.String("scriberr-id"), Value: aws.String(jobID)})
	_, err = u.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:  outputBucket,
		Key:     aws.String(transcriptFilename),
		Body:    strings.NewReader(transcript),
		Tagging: aws.String(tagsToS3TaggingString(tags)),
	})

	if err != nil {
		return fmt.Errorf("failed to upload to S3: %w", err)
	}

	logger.Info("Uploaded transcription result to S3", "bucket", *outputBucket, "filename", transcriptFilename, "job_id", jobID)

	return nil
}

func (u *S3JobProcessor) publishNotifications(jobID string, event string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var processedJob models.TranscriptionJob
	if loadErr := database.DB.Where("id = ?", jobID).First(&processedJob).Error; loadErr != nil {
		logger.Error("Failed to load job", "job_id", jobID, "event", event, "error", loadErr)
		return
	}

	if eventErr := u.sendEventBridgeEvent(ctx, processedJob, event); eventErr != nil {
		logger.Error("Failed to send EventBridge event", "job_id", jobID, "event", event, "error", eventErr)
	}

	logger.Info("Job notifications published", "job_id", jobID, "event", event)
}

// sendEventBridgeEvent sends a job completion event to AWS EventBridge
func (u *S3JobProcessor) sendEventBridgeEvent(ctx context.Context, job models.TranscriptionJob, eventStatus string) error {
	eventBusName := os.Getenv("EVENTBRIDGE_BUS_NAME")
	source := os.Getenv("EVENTBRIDGE_SOURCE")
	if eventBusName == "" {
		eventBusName = "default"
	}

	if source == "" {
		source = DefaultEventBridgeSource
	}

	detailType := "Transcribe Job State Change"

	detail := map[string]interface{}{
		"TranscriptionJobName":   getJobName(job),
		"TranscriptionJobID":     job.ID,
		"TranscriptionJobStatus": eventStatus,
		"DeliveredAt":            time.Now().UTC().Format(time.RFC3339),
	}

	if job.Transcript != nil && eventStatus == "COMPLETED" {
		var result interfaces.TranscriptResult
		if err := json.Unmarshal([]byte(*job.Transcript), &result); err == nil {
			detail["Result"] = result
		}
	}

	detailJSON, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("failed to marshal event detail: %w", err)
	}

	_, err = u.eventBridgeClient.PutEvents(ctx, &eventbridge.PutEventsInput{
		Entries: []ebTypes.PutEventsRequestEntry{
			{
				EventBusName: aws.String(eventBusName),
				Source:       aws.String(source),
				DetailType:   aws.String(detailType),
				Detail:       aws.String(string(detailJSON)),
			},
		},
	})

	if err != nil {
		return fmt.Errorf("failed to put event to EventBridge: %w", err)
	}

	logger.Info("Sent event to EventBridge", "job_id", job.ID, "status", eventStatus, "event_bus", eventBusName)
	return nil
}

// ProcessJobWithProcess implements the enhanced JobProcessor interface with process registration
func (u *S3JobProcessor) ProcessJobWithProcess(ctx context.Context, jobID string, registerProcess func(*exec.Cmd)) error {
	logger.Info("Processing job with unified processor (with process registration)", "job_id", jobID)

	// Register a nil process for backward compatibility
	registerProcess(nil)

	return u.ProcessJob(ctx, jobID)
}

// GetUnifiedService returns the underlying unified service for direct access to new features
func (u *S3JobProcessor) GetUnifiedService() *UnifiedTranscriptionService {
	return u.unifiedProcessor.GetUnifiedService()
}

// GetSupportedModels returns all supported models through the new architecture
func (u *S3JobProcessor) GetSupportedModels() map[string]interface{} {
	return u.unifiedProcessor.GetSupportedModels()
}

// GetModelStatus returns the status of all models
func (u *S3JobProcessor) GetModelStatus(ctx context.Context) map[string]bool {
	return u.unifiedProcessor.GetModelStatus(ctx)
}

// ValidateModelParameters validates parameters for a specific model
func (u *S3JobProcessor) ValidateModelParameters(modelID string, params map[string]interface{}) error {
	return u.unifiedProcessor.ValidateModelParameters(modelID, params)
}

// InitEmbeddedPythonEnv initializes the Python environment for all adapters
func (u *S3JobProcessor) InitEmbeddedPythonEnv() error {
	ctx := context.Background()
	return u.GetUnifiedService().Initialize(ctx)
}

// GetSupportedLanguages returns supported languages from all models
func (u *S3JobProcessor) GetSupportedLanguages() []string {
	return u.unifiedProcessor.GetSupportedLanguages()
}

// TerminateMultiTrackJob terminates a multi-track job and all its individual track jobs
func (u *S3JobProcessor) TerminateMultiTrackJob(jobID string) error {
	return u.GetUnifiedService().TerminateMultiTrackJob(jobID)
}

// IsMultiTrackJob checks if a job is a multi-track job
func (u *S3JobProcessor) IsMultiTrackJob(jobID string) bool {
	return u.GetUnifiedService().IsMultiTrackJob(jobID)
}

// tagsToS3TaggingString converts a map of tags to S3 tagging string format
func tagsToS3TaggingString(tags []types.Tag) string {
	if len(tags) == 0 {
		return ""
	}

	var tagPairs []string
	for _, tag := range tags {
		encodedKey := url.QueryEscape(*tag.Key)
		encodedValue := url.QueryEscape(*tag.Value)
		tagPairs = append(tagPairs, fmt.Sprintf("%s=%s", encodedKey, encodedValue))
	}

	return strings.Join(tagPairs, "&")
}

// downloadS3File downloads a file from S3 uri
func (u *S3JobProcessor) downloadS3File(ctx context.Context, uri string, saveTo string) error {
	if !strings.HasPrefix(uri, "s3://") {
		return fmt.Errorf("invalid S3 URI: %s", uri)
	}

	// Remove s3:// prefix and split bucket and key
	trimmed := strings.TrimPrefix(uri, "s3://")
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid S3 URI format: %s", uri)
	}

	bucket := parts[0]
	key := parts[1]

	// Download the file
	result, err := u.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("failed to download from S3: %w", err)
	}
	defer result.Body.Close()

	// Create the output file
	outFile, err := os.Create(saveTo)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer outFile.Close()

	// Copy the content
	_, err = outFile.ReadFrom(result.Body)
	if err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

func getJobName(job models.TranscriptionJob) string {
	if job.Title != nil {
		return *job.Title
	}

	return job.ID
}
