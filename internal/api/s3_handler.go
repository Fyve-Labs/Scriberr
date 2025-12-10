package api

import (
	"context"
	"net/http"
	"scriberr/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// S3TranscriptionRequest represents the S3 job request
type S3TranscriptionRequest struct {
	URI          string  `json:"uri"`
	OutputBucket *string `json:"output_bucket,omitempty"`
}

// @Summary Submit S3 transcription job
// @Description Submits transcription job with audio stored in S3
// @Tags config
// @Accept json
// @Produce json
// @Param request body S3TranscriptionRequest true "API Key"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} map[string]string
// @Failure 401 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/v1/transcription/s3 [post]
// @Security ApiKeyAuth
// @Security BearerAuth
func (h *Handler) SubmitS3Transcription(c *gin.Context) {
	var req S3TranscriptionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request data"})
		return
	}

	if req.URI == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "URI is required"})
	}

	profile := h.getDefaultProfile(c.Request.Context())
	if profile == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create job. Default profile not found."})
		return
	}

	job := models.TranscriptionJob{
		ID:           uuid.New().String(),
		AudioPath:    req.URI,
		AudioUri:     &req.URI,
		OutputBucket: req.OutputBucket,
		Parameters:   profile.Parameters,
		Diarization:  profile.Parameters.Diarize,
		Status:       models.StatusPending,
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
		"job_id": true,
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
