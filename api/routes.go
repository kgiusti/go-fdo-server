// SPDX-FileCopyrightText: (C) 2024 Intel Corporation
// SPDX-License-Identifier: Apache 2.0

package api

import (
	"net/http"

	"golang.org/x/time/rate"

	transport "github.com/fido-device-onboard/go-fdo/http"
	"github.com/fido-device-onboard/go-fdo/protocol"
	"github.com/fido-device-onboard/go-fdo/sqlite"
	"github.com/kgiusti/go-fdo-server/api/handlers"
)

// HTTPHandler handles HTTP requests
type HTTPHandler struct {
	handler *transport.Handler
	rvInfo  *[][]protocol.RvInstruction
	state   *sqlite.DB
}

func rateLimitMiddleware(limiter *rate.Limiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !limiter.Allow() {
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// NewHTTPHandler creates a new HTTPHandler
func NewHTTPHandler(handler *transport.Handler, rvInfo *[][]protocol.RvInstruction, state *sqlite.DB) *HTTPHandler {
	return &HTTPHandler{handler: handler, rvInfo: rvInfo, state: state}
}

// RegisterRoutes registers the routes for the HTTP server
func (h *HTTPHandler) RegisterRoutes() *http.ServeMux {
	handler := http.NewServeMux()
	limiter := rate.NewLimiter(2, 10)

	handler.Handle("POST /fdo/101/msg/{msg}", h.handler)
	handler.HandleFunc("/api/v1/rvinfo", func(w http.ResponseWriter, r *http.Request) {
		rateLimitMiddleware(limiter, http.HandlerFunc(handlers.RvInfoHandler(h.rvInfo))).ServeHTTP(w, r)
	})
	handler.HandleFunc("/api/v1/owner/redirect", func(w http.ResponseWriter, r *http.Request) {
		rateLimitMiddleware(limiter, http.HandlerFunc(handlers.OwnerInfoHandler)).ServeHTTP(w, r)
	})
	handler.HandleFunc("/api/v1/to0/", func(w http.ResponseWriter, r *http.Request) {
		rateLimitMiddleware(limiter, http.HandlerFunc(handlers.To0Handler(h.rvInfo, h.state))).ServeHTTP(w, r)
	})
	handler.HandleFunc("/api/v1/vouchers", func(w http.ResponseWriter, r *http.Request) {
		rateLimitMiddleware(limiter, http.HandlerFunc(handlers.GetVoucherHandler)).ServeHTTP(w, r)
	})
	handler.HandleFunc("/api/v1/owner/vouchers", func(w http.ResponseWriter, r *http.Request) {
		rateLimitMiddleware(limiter, http.HandlerFunc(handlers.InsertVoucherHandler(h.rvInfo))).ServeHTTP(w, r)
	})
	handler.HandleFunc("/health", handlers.HealthHandler)
	return handler
}
