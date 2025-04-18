package rest

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/lxc/incus-os/incus-osd/internal/rest/response"
)

func (s *Server) apiSystem(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPut {
		// If none of the supported methods, return NotImplemented.
		_ = response.NotImplemented(nil).Render(w)

		return
	}

	type reqSystem struct {
		Action string `json:"action"`
	}

	var req reqSystem
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		_ = response.InternalError(err).Render(w)

		return
	}

	switch req.Action {
	case "shutdown", "poweroff":
		close(s.state.TriggerShutdown)
	case "reboot":
		close(s.state.TriggerReboot)
	default:
		_ = response.BadRequest(fmt.Errorf("invalid action %q", req.Action)).Render(w)

		return
	}

	_ = response.EmptySyncResponse.Render(w)
}
