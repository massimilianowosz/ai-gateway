package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/auth"
	"github.com/ubiquum-ai/ubiquum-ai-gateway/internal/provider"
)

// ImageGenerationsHandler handles POST /v1/images/generations.
type ImageGenerationsHandler struct {
	registry *provider.Registry
	logger   *slog.Logger
}

// NewImageGenerationsHandler creates a new image generation handler.
func NewImageGenerationsHandler(registry *provider.Registry, logger *slog.Logger) *ImageGenerationsHandler {
	return &ImageGenerationsHandler{registry: registry, logger: logger}
}

func (h *ImageGenerationsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model          string `json:"model"`
		Prompt         string `json:"prompt"`
		N              *int   `json:"n,omitempty"`
		Size           string `json:"size,omitempty"`
		Quality        string `json:"quality,omitempty"`
		ResponseFormat string `json:"response_format,omitempty"`
		Style          string `json:"style,omitempty"`
		User           string `json:"user,omitempty"`
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20)) // 10MB limit
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "failed to read body")
		return
	}

	if err := json.Unmarshal(body, &req); err != nil {
		writeDecodeError(w, err)
		return
	}

	if req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "prompt is required")
		return
	}

	// Default model
	if req.Model == "" {
		req.Model = "dall-e-3"
	}

	// Enforce model access control — deny_models/deny_providers/residency,
	// not just the allow-list the previous hand-rolled check here missed
	// entirely (GW-02).
	if !checkModelAccess(w, r, h.registry, req.Model) {
		return
	}

	// A key that cannot pay — never funded, or funded and spent — may only
	// reach a model the gateway does not pay for. Checked here, where the model
	// is known: authentication cannot see it.
	if status := auth.CheckBudget(r.Context(), h.registry.IsGatewayBilled(req.Model)); status != auth.BudgetOK {
		writeBudgetError(w, status, req.Model)
		return
	}

	dep, err := getAuthorizedDeployment(r.Context(), h.registry, req.Model)
	if err != nil {
		writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q is not available", req.Model))
		return
	}

	generator, ok := dep.Provider.(provider.ImageGenerator)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("model %q does not support image generation", req.Model))
		return
	}

	resp, err := generator.GenerateImage(r.Context(), body)
	if err != nil {
		h.logger.Error("image generation error", "error", err)
		if ue, ok := err.(*provider.UpstreamError); ok {
			status := ue.StatusCode
			if status < 400 || status >= 600 {
				status = http.StatusBadGateway
			}
			writeError(w, status, "upstream_error", ue.Message)
			return
		}
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

// AudioSpeechHandler handles POST /v1/audio/speech (TTS).
type AudioSpeechHandler struct {
	registry *provider.Registry
	logger   *slog.Logger
}

// NewAudioSpeechHandler creates a new TTS handler.
func NewAudioSpeechHandler(registry *provider.Registry, logger *slog.Logger) *AudioSpeechHandler {
	return &AudioSpeechHandler{registry: registry, logger: logger}
}

func (h *AudioSpeechHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "failed to read body")
		return
	}

	var req struct {
		Model string `json:"model"`
		Input string `json:"input"`
		Voice string `json:"voice"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeDecodeError(w, err)
		return
	}

	if req.Model == "" {
		req.Model = "tts-1"
	}
	if req.Input == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "input is required")
		return
	}

	// Enforce model access control — this endpoint had no check at all
	// before (GW-02): a key's model_allowlist/denied_models/denied_
	// providers/residency were silently unenforced for text-to-speech.
	if !checkModelAccess(w, r, h.registry, req.Model) {
		return
	}

	// A key that cannot pay — never funded, or funded and spent — may only
	// reach a model the gateway does not pay for. Checked here, where the model
	// is known: authentication cannot see it.
	if status := auth.CheckBudget(r.Context(), h.registry.IsGatewayBilled(req.Model)); status != auth.BudgetOK {
		writeBudgetError(w, status, req.Model)
		return
	}

	dep, err := getAuthorizedDeployment(r.Context(), h.registry, req.Model)
	if err != nil {
		writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q is not available", req.Model))
		return
	}

	speaker, ok := dep.Provider.(provider.TextToSpeech)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("model %q does not support text-to-speech", req.Model))
		return
	}

	audioData, contentType, err := speaker.Speak(r.Context(), body)
	if err != nil {
		h.logger.Error("TTS error", "error", err)
		if ue, ok := err.(*provider.UpstreamError); ok {
			status := ue.StatusCode
			if status < 400 || status >= 600 {
				status = http.StatusBadGateway
			}
			writeError(w, status, "upstream_error", ue.Message)
			return
		}
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}

	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(audioData)
}

// AudioTranscriptionHandler handles POST /v1/audio/transcriptions (STT).
// Supports multipart/form-data with audio file upload.
type AudioTranscriptionHandler struct {
	registry *provider.Registry
	logger   *slog.Logger
}

// NewAudioTranscriptionHandler creates a new STT handler.
func NewAudioTranscriptionHandler(registry *provider.Registry, logger *slog.Logger) *AudioTranscriptionHandler {
	return &AudioTranscriptionHandler{registry: registry, logger: logger}
}

func (h *AudioTranscriptionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Parse multipart form (max 25MB for audio files)
	if err := r.ParseMultipartForm(25 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid multipart form: "+err.Error())
		return
	}

	model := r.FormValue("model")
	if model == "" {
		model = "whisper-1"
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "file is required")
		return
	}
	defer file.Close()

	// Enforce model access control — this endpoint had no check at all
	// before (GW-02): a key's model_allowlist/denied_models/denied_
	// providers/residency were silently unenforced for transcription.
	if !checkModelAccess(w, r, h.registry, model) {
		return
	}

	// A key that cannot pay — never funded, or funded and spent — may only
	// reach a model the gateway does not pay for. Checked here, where the model
	// is known: authentication cannot see it.
	if status := auth.CheckBudget(r.Context(), h.registry.IsGatewayBilled(model)); status != auth.BudgetOK {
		writeBudgetError(w, status, model)
		return
	}

	dep, err := getAuthorizedDeployment(r.Context(), h.registry, model)
	if err != nil {
		writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q is not available", model))
		return
	}

	transcriber, ok := dep.Provider.(provider.Transcriber)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("model %q does not support transcription", model))
		return
	}

	audioData, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "failed to read audio file")
		return
	}

	resp, err := transcriber.Transcribe(r.Context(), &provider.TranscriptionRequest{
		Model:    dep.ProviderModel,
		File:     audioData,
		Filename: header.Filename,
		Language: r.FormValue("language"),
		Format:   r.FormValue("response_format"),
	})
	if err != nil {
		h.logger.Error("transcription error", "error", err)
		if ue, ok := err.(*provider.UpstreamError); ok {
			status := ue.StatusCode
			if status < 400 || status >= 600 {
				status = http.StatusBadGateway
			}
			writeError(w, status, "upstream_error", ue.Message)
			return
		}
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

// PassthroughHandler provides a generic multipart/JSON passthrough to an upstream provider.
// Used for endpoints that just need to forward the request as-is.
type PassthroughHandler struct {
	registry *provider.Registry
	logger   *slog.Logger
	endpoint string // e.g., "/images/edits", "/images/variations"
}

// NewPassthroughHandler creates a generic forwarding handler.
func NewPassthroughHandler(registry *provider.Registry, logger *slog.Logger, endpoint string) *PassthroughHandler {
	return &PassthroughHandler{registry: registry, logger: logger, endpoint: endpoint}
}

func (h *PassthroughHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Read the full request body
	body, err := io.ReadAll(io.LimitReader(r.Body, 25<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "failed to read body")
		return
	}

	// Try to extract model from JSON body, or use a default
	model := "dall-e-2" // default for image edits/variations
	var jsonBody map[string]interface{}
	if json.Unmarshal(body, &jsonBody) == nil {
		if m, ok := jsonBody["model"].(string); ok && m != "" {
			model = m
		}
	}

	// Enforce model access control — this endpoint had no check at all
	// before (GW-02): a key's model_allowlist/denied_models/denied_
	// providers/residency were silently unenforced for image edits/
	// variations and any other endpoint routed through this passthrough.
	if !checkModelAccess(w, r, h.registry, model) {
		return
	}

	// A key that cannot pay — never funded, or funded and spent — may only
	// reach a model the gateway does not pay for. Checked here, where the model
	// is known: authentication cannot see it.
	if status := auth.CheckBudget(r.Context(), h.registry.IsGatewayBilled(model)); status != auth.BudgetOK {
		writeBudgetError(w, status, model)
		return
	}

	dep, err := getAuthorizedDeployment(r.Context(), h.registry, model)
	if err != nil {
		writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q is not available", model))
		return
	}

	forwarder, ok := dep.Provider.(provider.RawForwarder)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("model %q does not support this endpoint", model))
		return
	}

	respBody, respContentType, err := forwarder.Forward(r.Context(), h.endpoint, r.Header.Get("Content-Type"), bytes.NewReader(body))
	if err != nil {
		h.logger.Error("passthrough error", "endpoint", h.endpoint, "error", err)
		if ue, ok := err.(*provider.UpstreamError); ok {
			writeError(w, ue.StatusCode, "upstream_error", ue.Message)
			return
		}
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}

	w.Header().Set("Content-Type", respContentType)
	_, _ = w.Write(respBody)
}
