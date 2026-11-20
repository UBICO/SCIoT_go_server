package server

import (
	"net/http"
	"slices"
	"strings"

	"github.com/UBICO/SCIoT_go_server/internal/api"
)

// GET /api/v1/admin/models: i modelli del registry per la dashboard, con
// quanti device registrati li usano (docs/ADMIN_API.md). Sola lettura: gli
// stessi dati di GET /api/v1/models sulla porta dei device.

// adminModel è un modello del registry più il conteggio dei device.
type adminModel struct {
	api.ModelEntry
	Devices int `json:"devices"` // device registrati con questo modello
}

type adminModelsResponse struct {
	Models []adminModel `json:"models"`
}

func (s *Server) handleAdminModels(w http.ResponseWriter, _ *http.Request) {
	perKey := make(map[string]int)
	for _, d := range s.devices.List() {
		perKey[d.Model.ModelKey]++
	}
	entries := s.registry.Models()
	// Models viene da una map: ordine casuale. Per chiave, come nella config.
	slices.SortFunc(entries, func(a, b api.ModelEntry) int { return strings.Compare(a.ModelKey, b.ModelKey) })
	out := make([]adminModel, 0, len(entries)) // [] e non null senza modelli
	for _, e := range entries {
		out = append(out, adminModel{ModelEntry: e, Devices: perKey[e.ModelKey]})
	}
	writeJSON(w, http.StatusOK, adminModelsResponse{Models: out})
}
