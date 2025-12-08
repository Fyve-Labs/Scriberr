package adapters

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"scriberr/internal/transcription/interfaces"
	"scriberr/pkg/logger"
	"strings"
	"time"
)

type WhisperxResult struct {
	Segments []struct {
		Start   float64 `json:"start"`
		End     float64 `json:"end"`
		Text    string  `json:"text"`
		Speaker *string `json:"speaker,omitempty"`
	} `json:"segments"`
	Word []struct {
		Start   float64 `json:"start"`
		End     float64 `json:"end"`
		Word    string  `json:"word"`
		Score   float64 `json:"score"`
		Speaker *string `json:"speaker,omitempty"`
	} `json:"word_segments,omitempty"`
	Language string `json:"language"`
	Text     string `json:"text,omitempty"`
}

type WhisperxInput struct {
	Audio           string            `json:"audio"`
	DownloadHeaders map[string]string `json:"download_headers,omitempty"`
}

type RunPodInput struct {
	Input WhisperxInput `json:"input"`
}

// RunPodAdapter is a mock implementation of TranscriptionAdapter
type RunPodAdapter struct {
	*BaseAdapter
	FunctionName   string
	RunPodAPIKey   string
	RunPodEndpoint string
}

type RunpodOption func(*RunPodAdapter)

func WithRunpodEndpoint(baseURL string) RunpodOption {
	return func(r *RunPodAdapter) {
		r.RunPodEndpoint = baseURL
	}
}

func WithRunpodApiKey(key string) RunpodOption {
	return func(r *RunPodAdapter) {
		r.RunPodAPIKey = key
	}
}

func NewRunPodAdapter(w *WhisperXAdapter, opts ...RunpodOption) *RunPodAdapter {
	baseAdapter := NewBaseAdapter(interfaces.WhisperRunpod, w.modelPath, w.capabilities, ExtendsWhisperXSchema(w))
	endpoint := "http://localhost:8081"
	if val := os.Getenv("RUNPOD_ENDPOINT"); val != "" {
		endpoint = val
	}

	adapter := &RunPodAdapter{
		BaseAdapter:    baseAdapter,
		RunPodEndpoint: endpoint,
		RunPodAPIKey:   os.Getenv("RUNPOD_API_KEY"),
	}

	for _, opt := range opts {
		opt(adapter)
	}

	return adapter
}

func (m *RunPodAdapter) GetCapabilities() interfaces.ModelCapabilities {
	return interfaces.ModelCapabilities{
		ModelID:     interfaces.WhisperRunpod,
		ModelFamily: interfaces.WhisperRunpod,
	}
}

// GetSupportedModels returns the list of Whisper models supported
func (w *RunPodAdapter) GetSupportedModels() []string {
	return []string{
		"tiny", "tiny.en",
		"base", "base.en",
		"small", "small.en",
		"medium", "medium.en",
		"large", "large-v1", "large-v2", "large-v3",
	}
}

func (m *RunPodAdapter) PrepareEnvironment(ctx context.Context) error {
	return nil
}

func (m *RunPodAdapter) Transcribe(ctx context.Context, input interfaces.AudioInput, params map[string]interface{}, procCtx interfaces.ProcessingContext) (*interfaces.TranscriptResult, error) {
	startTime := time.Now()
	m.LogProcessingStart(input, procCtx)
	defer func() {
		m.LogProcessingEnd(procCtx, time.Since(startTime), nil)
	}()

	if err := m.ValidateParameters(params); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	logger.Debug("Executing Runpod", "endpoint", m.RunPodEndpoint)
	audioBytes, err := os.ReadFile(input.FilePath)
	if err != nil {
		return nil, fmt.Errorf("read audio file: %w", err)
	}
	encodedAudio := base64.StdEncoding.EncodeToString(audioBytes)
	params["audio_base64"] = encodedAudio

	ret, err := m.request(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("Runpod request: %w", err)
	}

	// Parse result
	result, err := m.parseResult(ret)
	if err != nil {
		return nil, fmt.Errorf("parse result: %w", err)
	}

	result.ProcessingTime = time.Since(startTime)
	result.ModelUsed = m.GetStringParameter(params, "model")
	result.Metadata = m.CreateDefaultMetadata(params)

	logger.Info("Runpod transcription completed",
		"segments", len(result.Segments),
		"words", len(result.WordSegments),
		"processing_time", result.ProcessingTime)

	return result, nil
}

func ExtendsWhisperXSchema(w *WhisperXAdapter) []interfaces.ParameterSchema {
	return append(w.schema,
		interfaces.ParameterSchema{
			Name:     "no_align",
			Type:     "bool",
			Required: false,
			Default:  false,
			Group:    "advance",
		},
		interfaces.ParameterSchema{
			Name:     "return_char_alignments",
			Type:     "bool",
			Required: false,
			Default:  false,
			Group:    "advance",
		},
		interfaces.ParameterSchema{
			Name:     "compression_ratio_threshold",
			Type:     "float",
			Required: false,
			Default:  2.4,
			Group:    "advance",
		},
		interfaces.ParameterSchema{
			Name:     "logprob_threshold",
			Type:     "float",
			Required: false,
			Default:  -1.0,
			Group:    "advance",
		},
		interfaces.ParameterSchema{
			Name:     "no_speech_threshold",
			Type:     "float",
			Required: false,
			Default:  0.6,
			Group:    "advance",
		},
		interfaces.ParameterSchema{
			Name:     "suppress_tokens",
			Type:     "string",
			Required: false,
			Default:  "-1",
			Group:    "advance",
		},
		interfaces.ParameterSchema{
			Name:     "chunk_size",
			Type:     "int",
			Required: false,
			Default:  30,
			Group:    "advance",
		},
		interfaces.ParameterSchema{
			Name:     "length_penalty",
			Type:     "int",
			Required: false,
			Default:  1,
			Group:    "advance",
		},
	)
}

func (m *RunPodAdapter) request(ctx context.Context, params map[string]interface{}) ([]byte, error) {
	jsonData, err := json.Marshal(&struct {
		Input map[string]interface{} `json:"input"`
	}{
		Input: params,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/runsync", m.RunPodEndpoint), bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	if m.RunPodAPIKey != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", m.RunPodAPIKey))
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

func (m *RunPodAdapter) parseResult(data []byte) (*interfaces.TranscriptResult, error) {
	var runpodResult struct {
		ID     string         `json:"id"`
		Status string         `json:"status"`
		Output WhisperxResult `json:"output"`
	}
	err := json.Unmarshal(data, &runpodResult)
	if err != nil {
		return nil, err
	}

	whisperxResult := runpodResult.Output

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
