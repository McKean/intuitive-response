// Package admin serves the /_irp/ inspection endpoints and /healthz.
package admin

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/McKean/intuitive-response/internal/logx"
	"github.com/McKean/intuitive-response/internal/presets"
)

func Register(mux *http.ServeMux, store *presets.Store, stats *logx.Stats) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /_irp/stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, stats.Snapshot())
	})
	mux.HandleFunc("GET /_irp/presets", func(w http.ResponseWriter, r *http.Request) {
		list := store.List(r.URL.Query().Get("session"))
		if list == nil {
			list = []presets.Preset{}
		}
		writeJSON(w, http.StatusOK, list)
	})
	mux.HandleFunc("POST /_irp/presets", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Session       string  `json:"session"`
			ExpectedInput string  `json:"expected_input"`
			Response      string  `json:"response"`
			MaxTurns      int     `json:"max_turns"`
			MinConfidence float64 `json:"min_confidence"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if in.Session == "" || strings.TrimSpace(in.ExpectedInput) == "" || strings.TrimSpace(in.Response) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session, expected_input and response are required"})
			return
		}
		p, c := store.Add(presets.Preset{
			SessionID:     in.Session,
			ExpectedInput: strings.TrimSpace(in.ExpectedInput),
			Response:      in.Response,
			TurnsLeft:     min(in.MaxTurns, 3),
			MinConfidence: in.MinConfidence,
			Source:        "admin",
		})
		stats.Add("presets_created", c.Created)
		stats.Add("presets_evicted", c.Evicted)
		writeJSON(w, http.StatusCreated, p)
	})
	mux.HandleFunc("DELETE /_irp/presets/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !store.Delete(r.PathValue("id")) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /_irp/presets", func(w http.ResponseWriter, r *http.Request) {
		session := r.URL.Query().Get("session")
		if session == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session is required"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"deleted": store.DeleteSession(session)})
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
