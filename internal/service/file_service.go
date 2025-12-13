package service

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
)

// FileService handles file system operations
type FileService interface {
	SaveUpload(file *multipart.FileHeader, destDir string) (string, error)
	CreateDirectory(path string) error
	RemoveFile(path string) error
	RemoveDirectory(path string) error
	ReadFile(path string) ([]byte, error)
	FileExists(path string) (bool, error)
	DownloadFile(ctx context.Context, url string, saveTo string) error
}

type fileService struct {
	s3Client *s3.Client

	// downloadedFiles stores the absolute file path and the time it was created
	downloadedFiles    map[string]time.Time
	downloadedFilesMux sync.Mutex
}

func NewFileService() FileService {
	ctx := context.Background()
	cfg, _ := config.LoadDefaultConfig(ctx)
	client := s3.NewFromConfig(cfg)
	fs := &fileService{
		s3Client:        client,
		downloadedFiles: make(map[string]time.Time),
	}

	fs.startDownloadedFilesCleanup()
	return fs
}

func (s *fileService) SaveUpload(fileHeader *multipart.FileHeader, destDir string) (string, error) {
	// Create directory if it doesn't exist
	if err := s.CreateDirectory(destDir); err != nil {
		return "", err
	}

	// Generate unique filename
	id := uuid.New().String()
	ext := filepath.Ext(fileHeader.Filename)
	filename := fmt.Sprintf("%s%s", id, ext)
	filePath := filepath.Join(destDir, filename)

	// Open source file
	src, err := fileHeader.Open()
	if err != nil {
		return "", fmt.Errorf("failed to open source file: %w", err)
	}
	defer src.Close()

	// Create destination file
	dst, err := os.Create(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to create destination file: %w", err)
	}
	defer dst.Close()

	// Copy content
	if _, err = io.Copy(dst, src); err != nil {
		os.Remove(filePath) // Clean up on error
		return "", fmt.Errorf("failed to copy file content: %w", err)
	}

	return filePath, nil
}

func (s *fileService) CreateDirectory(path string) error {
	if err := os.MkdirAll(path, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", path, err)
	}
	return nil
}

func (s *fileService) RemoveFile(path string) error {
	return os.Remove(path)
}

func (s *fileService) RemoveDirectory(path string) error {
	return os.RemoveAll(path)
}

func (s *fileService) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (s *fileService) FileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (s *fileService) DownloadFile(ctx context.Context, url string, saveTo string) error {
	if strings.HasPrefix(url, "s3://") {
		return s.downloadS3File(ctx, url, saveTo)
	}

	// Download using HTTP/HTTPS
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// Create the output file
	outFile, err := os.Create(saveTo)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer outFile.Close()

	_, err = io.Copy(outFile, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	s.saveDownloadedFiles(saveTo)

	return nil
}

func (s *fileService) downloadS3File(ctx context.Context, url string, saveTo string) error {
	trimmed := strings.TrimPrefix(url, "s3://")
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid S3 URI format: %s", url)
	}

	bucket := parts[0]
	key := parts[1]

	result, err := s.s3Client.GetObject(ctx, &s3.GetObjectInput{
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

	_, err = outFile.ReadFrom(result.Body)
	if err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	s.saveDownloadedFiles(saveTo)

	return nil
}

func (s *fileService) saveDownloadedFiles(saveTo string) {
	s.downloadedFilesMux.Lock()
	if s.downloadedFiles == nil {
		s.downloadedFiles = make(map[string]time.Time)
	}
	s.downloadedFiles[saveTo] = time.Now()
	s.downloadedFilesMux.Unlock()
}

// startDownloadedFilesCleanup launches a background goroutine that periodically
// deletes downloaded files older than 3 hours and prunes them from the map.
func (s *fileService) startDownloadedFilesCleanup() {
	const (
		maxAge   = 3 * time.Hour
		interval = 15 * time.Minute
	)
	ticker := time.NewTicker(interval)
	go func() {
		for range ticker.C {
			now := time.Now()
			var toDelete []string

			// Gather candidates under lock
			s.downloadedFilesMux.Lock()
			for path, createdAt := range s.downloadedFiles {
				if now.Sub(createdAt) > maxAge {
					toDelete = append(toDelete, path)
				}
			}
			s.downloadedFilesMux.Unlock()

			// Delete files and prune map
			s.downloadedFilesMux.Lock()
			for _, path := range toDelete {
				// Attempt to remove file; ignore error if it's already gone
				_ = os.Remove(path)
				delete(s.downloadedFiles, path)
			}
			s.downloadedFilesMux.Unlock()
		}
	}()
}
