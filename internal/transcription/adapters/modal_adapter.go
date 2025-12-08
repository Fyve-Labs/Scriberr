package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"scriberr/internal/transcription/interfaces"
	"scriberr/pkg/logger"
	"strings"
	"time"

	"github.com/modal-labs/libmodal/modal-go"
)

type RemoteAudioInput struct {
	FilePath string            `json:"file_path"`
	Format   string            `json:"format"`
	Headers  map[string]string `json:"headers,omitempty"`
}

// ModalAdapter is a mock implementation of TranscriptionAdapter
type ModalAdapter struct {
	*WhisperXAdapter
	client          *modal.Client
	FunctionName    string
	ScriberrBaseURL string
	ScriberrAPIKey  string
}

func NewModalAdapter(w *WhisperXAdapter, client *modal.Client, baseURL, apiKey string) *ModalAdapter {
	return &ModalAdapter{
		WhisperXAdapter: w,
		client:          client,
		FunctionName:    "scriberr-whisperx",
		ScriberrBaseURL: baseURL,
		ScriberrAPIKey:  apiKey,
	}
}
func (m *ModalAdapter) GetCapabilities() interfaces.ModelCapabilities {
	return interfaces.ModelCapabilities{
		ModelID:     interfaces.WhisperModal,
		ModelFamily: interfaces.WhisperModal,
	}
}

func (m *ModalAdapter) PrepareEnvironment(ctx context.Context) error {
	return nil
}

func (m *ModalAdapter) GetModelPath() string {
	return "/tmp/mock-model"
}

func (m *ModalAdapter) Transcribe(ctx context.Context, input interfaces.AudioInput, params map[string]interface{}, procCtx interfaces.ProcessingContext) (*interfaces.TranscriptResult, error) {
	startTime := time.Now()
	m.LogProcessingStart(input, procCtx)
	defer func() {
		m.LogProcessingEnd(procCtx, time.Since(startTime), nil)
	}()

	if err := m.ValidateParameters(params); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	transcribe, err := m.client.Functions.FromName(ctx, m.FunctionName, "transcribe", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get Function: %w", err)
	}

	logger.Debug("Executing Modal", "function", fmt.Sprintf("%s:transcribe", m.FunctionName))
	// Rewrite input
	remoteURL := fmt.Sprintf("%s/api/v1/transcription/%s/audio", m.ScriberrBaseURL, procCtx.JobID)
	remoteInput := &RemoteAudioInput{
		FilePath: remoteURL,
		Format:   input.Format,
		Headers: map[string]string{
			"x-api-key": m.ScriberrAPIKey,
		},
	}

	ret, err := transcribe.Remote(ctx, []any{procCtx.JobID, remoteInput, params}, nil)
	if err != nil {
		return nil, fmt.Errorf("call Modal function: %w", err)
	}

	// Parse result
	result, err := m.parseResult(ret)
	if err != nil {
		return nil, fmt.Errorf("failed to parse result: %w", err)
	}

	result.ProcessingTime = time.Since(startTime)
	result.ModelUsed = m.GetStringParameter(params, "model")
	result.Metadata = m.CreateDefaultMetadata(params)

	logger.Info("Modal Cloud transcription completed",
		"segments", len(result.Segments),
		"words", len(result.WordSegments),
		"processing_time", result.ProcessingTime)

	return result, nil
}

func (m *ModalAdapter) GetSupportedModels() []string {
	return []string{"modal-cloud"}
}

func (m *ModalAdapter) parseResult(ret any) (*interfaces.TranscriptResult, error) {
	// Parse WhisperX JSON format
	var whisperxResult WhisperxResult
	retStr, ok := ret.(string)
	if !ok {
		return nil, fmt.Errorf("response must be a json string")
	}

	err := json.Unmarshal([]byte(retStr), &whisperxResult)
	if err != nil {
		return nil, err
	}

	// Convert to standard format
	result := &interfaces.TranscriptResult{
		Language:     whisperxResult.Language,
		Segments:     make([]interfaces.TranscriptSegment, len(whisperxResult.Segments)),
		WordSegments: make([]interfaces.TranscriptWord, len(whisperxResult.Word)),
		Confidence:   0.0, // WhisperX doesn't provide overall confidence
	}

	// Convert segments
	var textParts []string
	for i, seg := range whisperxResult.Segments {
		result.Segments[i] = interfaces.TranscriptSegment{
			Start:   seg.Start,
			End:     seg.End,
			Text:    seg.Text,
			Speaker: seg.Speaker,
		}
		textParts = append(textParts, seg.Text)
	}

	// Convert words
	for i, word := range whisperxResult.Word {
		result.WordSegments[i] = interfaces.TranscriptWord{
			Start:   word.Start,
			End:     word.End,
			Word:    word.Word,
			Score:   word.Score,
			Speaker: word.Speaker,
		}
	}

	// Set full text
	if whisperxResult.Text != "" {
		result.Text = whisperxResult.Text
	} else {
		result.Text = strings.Join(textParts, " ")
	}

	return result, nil
}
