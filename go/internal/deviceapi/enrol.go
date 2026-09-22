package deviceapi

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type enrolRequest struct {
	Hostname     string `json:"hostname"`
	AgentVersion string `json:"agentVersion"`
}

type enrolResponse struct {
	DeviceID    string `json:"deviceId"`
	DeviceToken string `json:"deviceToken"`
}

// handleEnrol gives a machine its token. There is no shared secret: a
// hostname the console has not seen becomes a new, unassigned machine,
// exactly as a first status report used to; one it has seen gets a token
// only if it holds none, or an administrator has opened the door for it.
func (s *Server) handleEnrol(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if !s.limiter.allow(ip) {
		http.Error(w, "too many enrolments from this address; wait a minute", http.StatusTooManyRequests)
		return
	}
	if nets := s.enrolCIDRs(); len(nets) > 0 && !inAny(ip, nets) {
		http.Error(w, "enrolment is not accepted from this address", http.StatusForbidden)
		return
	}
	var req enrolRequest
	if !readJSON(w, r, &req) {
		return
	}
	req.Hostname = strings.TrimSpace(req.Hostname)
	if req.Hostname == "" || len(req.Hostname) > 64 || strings.ContainsAny(req.Hostname, " /\\\t\r\n") {
		http.Error(w, "a host name is required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	now := s.now()
	var out enrolResponse
	var created, reenrolled bool
	err := s.Store.InTx(ctx, func(tx repo.Store) error {
		before, err := tx.Devices().ByHostname(ctx, req.Hostname)
		switch {
		case errors.Is(err, repo.ErrNotFound):
			created = true
		case err != nil:
			return err
		case before.Status == repo.DeviceRevoked:
			// Forgotten by an administrator: it comes back as a new machine.
			created = true
		}
		device, err := tx.Devices().EnsureByHostname(ctx, req.Hostname)
		if err != nil {
			return err
		}
		if device.Status == repo.DeviceRevoked {
			if device, err = tx.Devices().Reactivate(ctx, device.ID); err != nil {
				return err
			}
		}
		if !created {
			live, err := tx.DeviceTokens().HasLive(ctx, device.ID, now)
			if err != nil {
				return err
			}
			if live && (device.ReenrolAllowedUntil == nil || device.ReenrolAllowedUntil.Before(now)) {
				return errAlreadyEnrolled
			}
			reenrolled = live
		}
		token, err := tx.DeviceTokens().Issue(ctx, device.ID)
		if err != nil {
			return err
		}
		if err := tx.Devices().SetEnrolled(ctx, device.ID, ip, now); err != nil {
			return err
		}
		if req.AgentVersion != "" {
			if err := tx.Devices().MarkSeen(ctx, device.ID, req.AgentVersion, now); err != nil {
				return err
			}
		}
		detail, _ := json.Marshal(map[string]any{"from": ip, "created": created, "reenrolled": reenrolled, "agentVersion": req.AgentVersion})
		if _, err := tx.Audit().Append(ctx, repo.AuditEvent{
			ActorType: "device", ActorID: "device:" + device.Hostname, Action: "device.enrol",
			TargetType: "device", TargetID: device.ID, After: detail, Result: "ok",
		}); err != nil {
			return err
		}
		out = enrolResponse{DeviceID: device.ID, DeviceToken: token}
		return nil
	})
	if errors.Is(err, errAlreadyEnrolled) {
		if s.Events != nil {
			s.Events.Ops("warn", "device_api", "enrolment refused: "+req.Hostname+" already holds a token", map[string]any{"hostname": req.Hostname, "from": ip})
		}
		http.Error(w, "this machine already holds a token; an administrator must allow re-enrolment", http.StatusConflict)
		return
	}
	if err != nil {
		s.fail(w, r, "enrol", err)
		return
	}
	if s.Events != nil {
		s.Events.Ops("info", "device_api", "enrolled "+req.Hostname, map[string]any{"hostname": req.Hostname, "from": ip, "created": created, "reenrolled": reenrolled})
	}
	writeJSON(w, http.StatusOK, out)
}

var errAlreadyEnrolled = errors.New("already enrolled")

func inAny(ip string, nets []net.IPNet) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}
