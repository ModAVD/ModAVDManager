// Copyright 2026 jinghero. All rights reserved.

package ui

import "net/http"

func (s *server) handleRegional(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok {
		return
	}
	result, err := s.mgr.Regional(r.Context(), i)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
