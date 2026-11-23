package broker

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// MinTokenLen is the shortest API bearer token Handler accepts.
const MinTokenLen = 16

// Handler returns the HTTP ticket API. Every route except /healthz requires
// "Authorization: Bearer <token>".
//
//	POST   /v1/tickets         Request JSON -> 201 Ticket JSON
//	GET    /v1/sessions        -> 200 [Session]
//	DELETE /v1/sessions/{id}   end a session or revoke a pending ticket -> 204
//	GET    /healthz            -> 200
//
// Ticket requests carry BMC credentials; serve the API over TLS or keep it on
// a network only the caller can reach.
func (b *Broker) Handler(token string) (http.Handler, error) {
	if len(token) < MinTokenLen {
		return nil, errors.New("broker: API token must be at least 16 characters")
	}
	want := sha256.Sum256([]byte(token))
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			sum := sha256.Sum256([]byte(got))
			if !ok || subtle.ConstantTimeCompare(sum[:], want[:]) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="bmc-broker"`)
				writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
				return
			}
			h(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("POST /v1/tickets", authed(b.handleIssue))
	mux.HandleFunc("GET /v1/sessions", authed(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, b.Sessions())
	}))
	mux.HandleFunc("DELETE /v1/sessions/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		if !b.Close(r.PathValue("id")) {
			writeError(w, http.StatusNotFound, "no such session or ticket")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	return mux, nil
}

func (b *Broker) handleIssue(w http.ResponseWriter, r *http.Request) {
	var req Request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	t, err := b.Issue(r.Context(), req)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, ErrInvalid):
			status = http.StatusBadRequest
		case errors.Is(err, ErrHostNotAllowed), errors.Is(err, ErrNotPermitted):
			status = http.StatusForbidden
		case errors.Is(err, ErrBusy):
			status = http.StatusConflict
		case errors.Is(err, ErrTooMany):
			status = http.StatusTooManyRequests
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
