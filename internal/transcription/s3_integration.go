package transcription

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"scriberr/internal/database"
	"scriberr/internal/models"
	"scriberr/internal/repository"
	"scriberr/pkg/logger"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const (
	DefaultSqsQueueURL  = "https://sqs.us-east-1.amazonaws.com/209479271613/prod-audio-ingest"
	DefaultOutputBucket = "dev-transcribe-output-209479271613-us-east-1"
)

// S3JobProcessor implements the existing JobProcessor interface using the new unified service
type S3JobProcessor struct {
	unifiedProcessor *UnifiedJobProcessor
	jobRepo          repository.JobRepository
	uploadDir        string
	outputBucket     string
	s3Client         *s3.Client
}

// NewS3JobProcessor creates a new job processor using the unified service
func NewS3JobProcessor(unifiedProcessor *UnifiedJobProcessor, jobRepo repository.JobRepository, uploadDir string) (*S3JobProcessor, error) {
	outputBucket := os.Getenv("TRANSCRIBE_OUTPUT_BUCKET")
	if outputBucket == "" {
		outputBucket = DefaultOutputBucket
	}

	// Load AWS configuration
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}

	client := s3.NewFromConfig(cfg)

	return &S3JobProcessor{
		s3Client:         client,
		uploadDir:        uploadDir,
		unifiedProcessor: unifiedProcessor,
		jobRepo:          jobRepo,
		outputBucket:     outputBucket,
	}, nil
}

// Initialize prepares the job processor
func (u *S3JobProcessor) Initialize(ctx context.Context) error {
	return u.unifiedProcessor.Initialize(ctx)
}

// ProcessJob implements the legacy JobProcessor interface
func (u *S3JobProcessor) ProcessJob(ctx context.Context, jobID string) error {
	job, err := u.jobRepo.FindByID(ctx, jobID)
	if err != nil {
		return err
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

	// Skip S3 upload if no transcript is available
	if transcript == "" {
		return nil
	}

	var outputBucket string
	if processedJob.OutputBucket != nil {
		outputBucket = *processedJob.OutputBucket
	}

	if outputBucket == "" {
		outputBucket = u.outputBucket
	}

	uid := strings.TrimSuffix(filename, filepath.Ext(filename))

	_, err = u.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(outputBucket),
		Key:    aws.String(fmt.Sprintf("%s.json", uid)),
		Body:   strings.NewReader(transcript),
	})

	if err != nil {
		return fmt.Errorf("failed to update to S3: %w", err)
	} else {
		logger.Info("Uploaded transcription result to S3", "bucket", u.outputBucket, "filename", fmt.Sprintf("%s.json", uid), "job_id", jobID)
	}

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
