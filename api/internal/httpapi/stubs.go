package httpapi

import "net/http"

// Filled in by the next slices.
func (s *Server) postMessage(w http.ResponseWriter, r *http.Request) { writeError(w, 501, "todo") }
func (s *Server) getChart(w http.ResponseWriter, r *http.Request)    { writeError(w, 501, "todo") }
func (s *Server) runTool(w http.ResponseWriter, r *http.Request)     { writeError(w, 501, "todo") }
func (s *Server) requireInternalToken(next http.Handler) http.Handler { return next }
