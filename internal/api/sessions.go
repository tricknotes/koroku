package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/tricknotes/koroku/internal/store"
)

// loginEventsShown is how much of the login history the console lists.
const loginEventsShown = 50

type sessionJSON struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	IP         string    `json:"ip"`
	UserAgent  string    `json:"user_agent"`
	Current    bool      `json:"current"`
	// User is whose session it is; the owner sees everyone's.
	User userRefJSON `json:"user"`
}

type loginEventJSON struct {
	At           time.Time `json:"at"`
	Outcome      string    `json:"outcome"`
	GitHubUserID int64     `json:"github_user_id"`
	Login        string    `json:"login"`
	IP           string    `json:"ip"`
	UserAgent    string    `json:"user_agent"`
}

// handleListSessions lists the browsers that are logged in as the user, or,
// for the owner, as anyone.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	out := []sessionJSON{}
	current, ok := sessionFrom(r)
	if !ok {
		// --insecure-no-auth: there are no sessions.
		writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
		return
	}
	list, err := s.store.ListSessions(r.Context(), recordsOf(r), s.clock.Now())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	for _, sess := range list {
		out = append(out, sessionJSON{
			ID: sess.ID, CreatedAt: sess.CreatedAt, LastSeenAt: sess.LastSeenAt, ExpiresAt: sess.ExpiresAt,
			IP: sess.IP, UserAgent: sess.UserAgent, Current: sess.TokenHash == current.TokenHash,
			User: userRefJSON{ID: sess.GitHubUserID, Login: sess.Login},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

// handleDeleteSession ends one session: one's own, or for the owner anyone's.
// Ending one's own is a logout.
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	current, ok := sessionFrom(r)
	if !ok {
		writeError(w, http.StatusNotFound, "session_not_found", "no such session")
		return
	}
	id := r.PathValue("id")
	err := s.store.DeleteSessionByID(r.Context(), recordsOf(r), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "session_not_found", "no such session")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if id == current.ID {
		s.clearCookie(w, sessionCookie)
	}
	s.log.Info("session revoked", "login", current.Login)
	w.WriteHeader(http.StatusNoContent)
}

// handleRevokeOtherSessions ends every session but the caller's.
func (s *Server) handleRevokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	current, ok := sessionFrom(r)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"revoked": 0})
		return
	}
	n, err := s.store.DeleteOtherSessions(r.Context(), current.GitHubUserID, current.TokenHash, s.clock.Now())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.log.Info("other sessions revoked", "login", current.Login, "count", n)
	writeJSON(w, http.StatusOK, map[string]any{"revoked": n})
}

// handleLoginEvents returns the recent login history: the user's own, or
// for the owner everyone's (including refused attempts).
func (s *Server) handleLoginEvents(w http.ResponseWriter, r *http.Request) {
	out := []loginEventJSON{}
	if s.auth.InsecureNoAuth {
		writeJSON(w, http.StatusOK, map[string]any{"events": out})
		return
	}
	events, err := s.store.ListLoginEvents(r.Context(), recordsOf(r), loginEventsShown)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	for _, ev := range events {
		out = append(out, loginEventJSON(ev))
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}
