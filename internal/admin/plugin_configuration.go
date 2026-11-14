package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Called only after the shared authentication, temporary-password and CSRF gates.
func (s *Server) pluginConfiguration(w http.ResponseWriter, r *http.Request) {
	if s.Backend.Initializing() {
		http.Error(w, "initializing or closing", 409)
		return
	}
	timeout := 180 * time.Second
	// Model connection tests may use the plugin's configurable 600-second request deadline.
	if r.URL.Path == "/api/plugin-action" {
		timeout = 660 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	var out any
	var err error
	switch {
	case r.URL.Path == "/api/plugin-config" && r.Method == "GET":
		out, err = s.Backend.PluginConfiguration(ctx, r.URL.Query().Get("id"))
	case r.URL.Path == "/api/plugin-config" && r.Method == "POST":
		var body struct{ ID, Revision, Values string }
		if err = requestJSON(r, &body); err == nil {
			err = s.Backend.SavePluginConfiguration(ctx, body.ID, body.Revision, body.Values)
		}
		out = map[string]bool{"saved": err == nil}
	case r.URL.Path == "/api/plugin-action" && r.Method == "POST":
		var body struct{ ID, Revision, Action, Input string }
		if err = requestJSON(r, &body); err == nil {
			var result string
			result, err = s.Backend.PluginConfigurationAction(ctx, body.ID, body.Revision, body.Action, body.Input)
			out = map[string]string{"message": result}
		}
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		problem(w, err)
		return
	}
	// Configuration schemas use explicit JSON types, not Core int64 normalization.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(out)
}
