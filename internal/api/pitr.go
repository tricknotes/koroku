package api

import (
	"context"
	"net/http"
	"time"

	"github.com/tricknotes/koroku/internal/pitr"
	"github.com/tricknotes/koroku/internal/store"
)

// PITR is the point-in-time recovery of the apps (ARCHITECTURE 10.9).
type PITR interface {
	Status(ctx context.Context, app store.App) (pitr.Status, error)
	// Disable turns the archiving of service off.
	Disable(ctx context.Context, app store.App, service string) error
}

type pitrGapJSON struct {
	From   time.Time  `json:"from"`
	To     *time.Time `json:"to"`
	Reason string     `json:"reason"`
}

type pitrStatusJSON struct {
	Service       string        `json:"service"`
	State         string        `json:"state"`
	Error         string        `json:"error"`
	Warning       string        `json:"warning"`
	EarliestAt    *time.Time    `json:"earliest_at"`
	ArchivedUntil *time.Time    `json:"archived_until"`
	Gaps          []pitrGapJSON `json:"gaps"`
}

func (s *Server) handlePITRStatus(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	out := pitrStatusJSON{Service: app.PITRService, State: pitr.StateOff, Gaps: []pitrGapJSON{}}
	if s.pitr == nil {
		if app.PITRService != "" {
			out.State = pitr.StateUnavailable
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	st, err := s.pitr.Status(r.Context(), app)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out.State, out.Error, out.Warning = st.State, st.Error, st.Warning
	out.EarliestAt, out.ArchivedUntil = st.EarliestAt, st.ArchivedUntil
	for _, g := range st.Gaps {
		out.Gaps = append(out.Gaps, pitrGapJSON{From: g.StartedAt, To: g.EndedAt, Reason: g.Reason})
	}
	writeJSON(w, http.StatusOK, out)
}
