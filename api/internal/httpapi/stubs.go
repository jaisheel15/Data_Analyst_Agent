package httpapi

import "net/http"

// Filled in by the next slice.
func (s *Server) postMessage(w http.ResponseWriter, r *http.Request) { writeError(w, 501, "todo") }
