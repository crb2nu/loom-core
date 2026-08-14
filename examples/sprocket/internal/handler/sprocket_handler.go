package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/crb2nu/sprocket/internal/repository"
	"github.com/crb2nu/sprocket/internal/service"
)

type SprocketHandler struct {
	service *service.SprocketService
	logger  *slog.Logger
}

func NewSprocketHandler(service *service.SprocketService, logger *slog.Logger) *SprocketHandler {
	return &SprocketHandler{
		service: service,
		logger:  logger,
	}
}

func (h *SprocketHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", h.health)
	mux.HandleFunc("GET /readyz", h.ready)
	mux.HandleFunc("POST /sprockets", h.create)
	mux.HandleFunc("GET /sprockets", h.list)
	mux.HandleFunc("GET /sprockets/{id}", h.get)
	mux.HandleFunc("DELETE /sprockets/{id}", h.delete)
}

func (h *SprocketHandler) Routes() http.Handler {
	mux := http.NewServeMux()
	h.Register(mux)
	return errorEnvelopeMiddleware(mux)
}

func (h *SprocketHandler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *SprocketHandler) ready(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (h *SprocketHandler) create(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	var input service.CreateSprocketInput
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	sprocket, err := h.service.Create(r.Context(), input)
	if errors.Is(err, service.ErrValidation) {
		writeError(w, http.StatusBadRequest, "invalid sprocket")
		return
	}
	if err != nil {
		h.logger.Error("create sprocket failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	writeJSON(w, http.StatusCreated, sprocket)
}

func (h *SprocketHandler) list(w http.ResponseWriter, r *http.Request) {
	sprockets, err := h.service.List(r.Context())
	if err != nil {
		h.logger.Error("list sprockets failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	writeJSON(w, http.StatusOK, sprockets)
}

func (h *SprocketHandler) get(w http.ResponseWriter, r *http.Request) {
	sprocket, err := h.service.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, repository.ErrNotFound) {
		writeError(w, http.StatusNotFound, "sprocket not found")
		return
	}
	if err != nil {
		h.logger.Error("get sprocket failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	writeJSON(w, http.StatusOK, sprocket)
}

func (h *SprocketHandler) delete(w http.ResponseWriter, r *http.Request) {
	err := h.service.Delete(r.Context(), r.PathValue("id"))
	if errors.Is(err, repository.ErrNotFound) {
		writeError(w, http.StatusNotFound, "sprocket not found")
		return
	}
	if err != nil {
		h.logger.Error("delete sprocket failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

type captureResponseWriter struct {
	headers http.Header
	body    bytes.Buffer
	status  int
}

func newCaptureResponseWriter() *captureResponseWriter {
	return &captureResponseWriter{
		headers: make(http.Header),
	}
}

func (w *captureResponseWriter) Header() http.Header {
	return w.headers
}

func (w *captureResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *captureResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(body)
}

func errorEnvelopeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture := newCaptureResponseWriter()
		next.ServeHTTP(capture, r)

		status := capture.status
		if status == 0 {
			status = http.StatusOK
		}

		contentType := capture.headers.Get("Content-Type")
		if status >= http.StatusBadRequest && contentType != "application/json" {
			writeError(w, status, http.StatusText(status))
			return
		}

		for key, values := range capture.headers {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(status)
		if capture.body.Len() > 0 {
			_, _ = w.Write(capture.body.Bytes())
		}
	})
}
