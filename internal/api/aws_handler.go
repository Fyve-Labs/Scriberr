package api

import (
	"context"
	"encoding/json"
	"net/http"
	"scriberr/internal/models"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/transcribe"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// @Summary Submit AWS transcribe compatible job
// @Description Submit AWS transcribe compatible job
// @Tags config
// @Accept json
// @Produce json
// @Param request body transcribe.StartTranscriptionJobInput true "API Key"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} map[string]string
// @Failure 401 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/v1/transcription/aws-transcribe [post]
// @Security ApiKeyAuth
// @Security BearerAuth
func (h *Handler) SubmitAWSTranscribeJob(c *gin.Context) {
	var req transcribe.StartTranscriptionJobInput
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request data"})
		return
	}

	if req.Media == nil || req.Media.MediaFileUri == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Media.MediaFileUri is required"})
	}

	profile := h.getDefaultProfile(c.Request.Context())
	if profile == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create job. Default profile not found."})
		return
	}

	mediaURI := *req.Media.MediaFileUri
	params := profile.Parameters
	if req.LanguageCode != "" {
		shortCode := strings.Split(string(req.LanguageCode), "-")[0]
		params.Language = &shortCode
	}

	params.Diarize = true
	var tags *string
	if len(req.Tags) > 0 {
		bytes, err := json.Marshal(req.Tags)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to marshal tags"})
			return
		}
		tags = aws.String(string(bytes))
	}

	job := models.TranscriptionJob{
		ID:               uuid.New().String(),
		AudioPath:        mediaURI,
		AudioUri:         &mediaURI,
		Title:            req.TranscriptionJobName,
		OutputBucketName: req.OutputBucketName,
		Parameters:       params,
		Diarization:      params.Diarize,
		Tags:             tags,
		Status:           models.StatusPending,
	}

	if err := h.jobRepo.Create(c.Request.Context(), &job); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create job"})
		return
	}

	// Enqueue the job for transcription
	if err := h.taskQueue.EnqueueJob(job.ID); err != nil {
		// If enqueueing fails, revert status but don't fail the upload
		job.Status = models.StatusUploaded
		_ = h.jobRepo.Update(c.Request.Context(), &job)
	}

	c.JSON(http.StatusOK, gin.H{
		"JobID":                job.ID,
		"TranscriptionJobName": job.Title,
		"TranscriptionProfile": profile.Name,
		"OutputBucketName":     job.OutputBucketName,
	})
}

func (h *Handler) getDefaultProfile(ctx context.Context) *models.TranscriptionProfile {
	profile, _ := h.profileRepo.FindDefault(ctx)
	if profile != nil {
		return profile
	}

	profiles, _, _ := h.profileRepo.List(ctx, 0, 1)
	if len(profiles) > 0 {
		profile = &profiles[0]
	}

	return profile
}
