package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"time"

	"example.com/wagering/internal/application"
	"example.com/wagering/internal/domain"
)

type Handler struct{ service *application.Service }

func New(service *application.Service) *Handler { return &Handler{service: service} }
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /wallets", h.createWallet)
	mux.HandleFunc("GET /wallets/{walletId}", h.wallet)
	mux.HandleFunc("GET /wallets/{walletId}/ledger", h.ledger)
	mux.HandleFunc("POST /wallets/{walletId}/reconciliation", h.reconcile)
	mux.HandleFunc("POST /wagering/transactions", h.process)
	mux.HandleFunc("GET /wagering/transactions/{transactionId}", h.transaction)
	mux.HandleFunc("GET /providers/{providerId}/wagering/transactions/{externalTransactionId}", h.providerTransaction)
}
func (h *Handler) reconcile(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := requestContext(r)
	defer cancel()
	result, err := h.service.Reconcile(ctx, r.PathValue("walletId"))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, result)
}
func write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	code := "TEMPORARILY_UNAVAILABLE"
	switch {
	case errors.Is(err, application.ErrInvalidInput):
		status = http.StatusBadRequest
		code = "INVALID_INPUT"
	case errors.Is(err, application.ErrConflict):
		status = http.StatusConflict
		code = "CONFLICT"
	case errors.Is(err, application.ErrNotFound):
		status = http.StatusNotFound
		code = "NOT_FOUND"
	}
	if status == http.StatusServiceUnavailable {
		slog.Error("request could not complete", "code", code)
	}
	write(w, status, map[string]string{"code": code})
}
func decode(w http.ResponseWriter, r *http.Request, value any) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return application.ErrInvalidInput
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(value); err != nil {
		return application.ErrInvalidInput
	}
	if err = decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return application.ErrInvalidInput
	}
	return nil
}
func requestContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 8*time.Second)
}
func (h *Handler) createWallet(w http.ResponseWriter, r *http.Request) {
	var input struct {
		PlayerID       string       `json:"playerId"`
		InitialBalance domain.Money `json:"initialBalance"`
	}
	if err := decode(w, r, &input); err != nil {
		fail(w, err)
		return
	}
	ctx, cancel := requestContext(r)
	defer cancel()
	result, err := h.service.CreateWallet(ctx, input.PlayerID, input.InitialBalance)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusCreated, result)
}
func (h *Handler) process(w http.ResponseWriter, r *http.Request) {
	var input domain.Operation
	if err := decode(w, r, &input); err != nil {
		fail(w, err)
		return
	}
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) != 1 {
		fail(w, application.ErrInvalidInput)
		return
	}
	ctx, cancel := requestContext(r)
	defer cancel()
	result, err := h.service.Process(ctx, keys[0], input)
	if err != nil {
		fail(w, err)
		return
	}
	status := http.StatusOK
	if result.Status == "PENDING_REFERENCE" {
		status = http.StatusAccepted
	}
	if result.Status == "REJECTED" {
		status = http.StatusUnprocessableEntity
	}
	write(w, status, result)
}
func (h *Handler) wallet(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := requestContext(r)
	defer cancel()
	result, err := h.service.Wallet(ctx, r.PathValue("walletId"))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, result)
}
func (h *Handler) transaction(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := requestContext(r)
	defer cancel()
	result, err := h.service.Transaction(ctx, r.PathValue("transactionId"))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, result)
}
func (h *Handler) providerTransaction(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := requestContext(r)
	defer cancel()
	result, err := h.service.ProviderTransaction(ctx, r.PathValue("providerId"), r.PathValue("externalTransactionId"))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, result)
}
func (h *Handler) ledger(w http.ResponseWriter, r *http.Request) {
	limit := 50
	var err error
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil {
			fail(w, application.ErrInvalidInput)
			return
		}
	}
	ctx, cancel := requestContext(r)
	defer cancel()
	result, err := h.service.Ledger(ctx, r.PathValue("walletId"), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, result)
}
