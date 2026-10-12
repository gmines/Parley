// Package httpx holds small JSON-over-HTTP helpers shared by Parley services.
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
)

const maxBodyBytes = 64 << 10

// Error is the JSON error body every endpoint returns.
type Error struct {
	Status  int         `json:"-"`
	Code    string      `json:"code"`
	Message string      `json:"message"`
	Header  http.Header `json:"-"` // extra response headers, e.g. Retry-After
}

func (e *Error) Error() string { return e.Message }

func NewError(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

var (
	ErrBadJSON      = NewError(http.StatusBadRequest, "bad_request", "request body is not valid JSON")
	ErrUnauthorized = NewError(http.StatusUnauthorized, "unauthorized", "missing or invalid credentials")
	ErrNotFound     = NewError(http.StatusNotFound, "not_found", "not found")
	errInternal     = NewError(http.StatusInternalServerError, "internal", "internal server error")
)

// DecodeJSON reads a JSON body into dst, rejecting unknown fields and oversized bodies.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return ErrBadJSON
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return ErrBadJSON
	}
	return nil
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		json.NewEncoder(w).Encode(v)
	}
}

// WriteError writes err as an *Error, or a generic 500 (and logs it) otherwise.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var e *Error
	if !errors.As(err, &e) {
		slog.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		e = errInternal
	}
	for k, v := range e.Header {
		w.Header()[k] = v
	}
	WriteJSON(w, e.Status, e)
}

// Handler adapts a handler that returns an error.
type Handler func(w http.ResponseWriter, r *http.Request) error

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h(w, r); err != nil {
		WriteError(w, r, err)
	}
}
