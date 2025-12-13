package api

import (
	"net/http"
	"os"
	"path/filepath"
	"scriberr/internal/database"
	"scriberr/internal/models"
	"scriberr/pkg/logger"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// @Summary Get audio file
// @Description Serve the audio file for a transcription job
// @Tags transcription
// @Produce audio/mpeg,audio/wav,audio/mp4
// @Param id path string true "Job ID"
// @Success 200 {file} binary
// @Failure 404 {object} map[string]string
// @Router /api/v1/transcription/{id}/audio [get]
// @Security ApiKeyAuth
func (h *Handler) GetAudioFileWrapper(decorated gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		jobID := c.Param("id")

		var job models.TranscriptionJob
		if err := database.DB.Where("id = ?", jobID).First(&job).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				c.JSON(http.StatusNotFound, gin.H{"error": "Job not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get job"})
			return
		}

		if job.AudioUri == nil || !strings.HasPrefix(*job.AudioUri, "s3://") {
			decorated(c)
			return
		}

		// Download audio file from S3
		filename := filepath.Base(*job.AudioUri)
		audioPath := filepath.Join(h.config.UploadDir, filename)
		if _, err := os.Stat(audioPath); os.IsNotExist(err) {
			logger.Debug("Downloading audio", "uri", *job.AudioUri, "audio_path", audioPath)
			err := h.fileService.DownloadFile(c.Request.Context(), *job.AudioUri, audioPath)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to download audio"})
				return
			}
		}

		job.AudioPath = audioPath
		if err := h.jobRepo.Update(c.Request.Context(), &job); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update audio path"})
			return
		}

		decorated(c)
	}
}
