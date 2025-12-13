package adapters

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"scriberr/internal/transcription/interfaces"
	"scriberr/pkg/logger"
	"strings"
	"time"

	"github.com/modal-labs/libmodal/modal-go"
)

type ModalAdapter struct {
	*BaseAdapter
	client       *modal.Client
	FunctionName string
}

func NewModalAdapter(w *WhisperXAdapter, client *modal.Client) *ModalAdapter {
	baseAdapter := NewBaseAdapter(interfaces.ModalWhisperX, w.modelPath, w.capabilities, ExtendsWhisperXSchema(w))

	appName := "scriberr-whisperx"
	if val := os.Getenv("MODAL_APP_NAME"); val != "" {
		appName = val
	}

	return &ModalAdapter{
		BaseAdapter:  baseAdapter,
		client:       client,
		FunctionName: appName,
	}
}

func (m *ModalAdapter) GetCapabilities() interfaces.ModelCapabilities {
	caps := m.BaseAdapter.GetCapabilities()
	caps.ModelID = interfaces.ModalWhisperX
	caps.ModelFamily = interfaces.ModalWhisperX
	return caps
}

func (m *ModalAdapter) PrepareEnvironment(ctx context.Context) error {
	return nil
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
	audioBytes, err := os.ReadFile(input.FilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read audio file: %w", err)
	}
	encodedAudio := base64.StdEncoding.EncodeToString(audioBytes)
	params["audio_base64"] = encodedAudio
	ret, err := transcribe.Remote(ctx, []any{procCtx.JobID, params}, nil)
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
		Confidence:   0.0,
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
