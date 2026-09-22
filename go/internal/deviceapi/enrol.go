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

// handleEnrol gives a machine its token. There is no shared secret, so the
// rule is about what a token would give access to:
//
//   - a hostname the console has never seen becomes a new, unassigned
//     machine, exactly as a first status report used to. Unassigned, it
//     gets policy and nothing secret;
//   - a machine the console knows but that was never assigned to anybody
//     and holds no token may enrol too: there is still nothing to take;
//   - anything else -- a machine somebody was ever assigned to, one that
//     already holds a token, one an administrator forgot -- is refused
//     until an administrator opens the window for it. Its name is what a
//     stranger would need to claim to reach its assignee's credentials,
//     and the migration of a fleet that has never enrolled is exactly
//     when every one of its machines is in this state.
func (s *Server) handleEnrol(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	origin := s.originIP(r)
	if !s.limiter.allow(ip) || !s.limiter.allow(globalLimitKey) {
		http.Error(w, "too many enrolments; wait a minute", http.StatusTooManyRequests)
		return
	}
	if nets := s.enrolCIDRs(); len(nets) > 0 && !inAny(s.originIP(r), nets) {
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
		}
		device, err := tx.Devices().EnsureByHostname(ctx, req.Hostname)
		if err != nil {
			return err
		}
		if !created {
			windowOpen := before.ReenrolAllowedUntil != nil && before.ReenrolAllowedUntil.After(now)
			if !windowOpen {
				live, err := tx.DeviceTokens().HasLive(ctx, device.ID, now)
				if err != nil {
					return err
				}
				history, err := tx.Bindings().History(ctx, device.ID)
				if err != nil {
					return err
				}
				if live || len(history) > 0 || before.Status == repo.DeviceRevoked || before.Channel == repo.ChannelAPI {
					return errAlreadyEnrolled
				}
			}
			reenrolled = true
			if device.Status == repo.DeviceRevoked {
				if device, err = tx.Devices().Reactivate(ctx, device.ID); err != nil {
					return err
				}
			}
		}
		token, err := tx.DeviceTokens().Issue(ctx, device.ID)
		if err != nil {
			return err
		}
		if err := tx.Devices().SetEnrolled(ctx, device.ID, origin, now); err != nil {
			return err
		}
		if req.AgentVersion != "" {
			if err := tx.Devices().MarkSeen(ctx, device.ID, req.AgentVersion, now); err != nil {
				return err
			}
		}
		detail, _ := json.Marshal(map[string]any{"from": origin, "created": created, "reenrolled": reenrolled, "agentVersion": req.AgentVersion})
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
			s.Events.Ops("warn", "device_api", "enrolment refused: "+req.Hostname+" is known and not open for enrolment", map[string]any{"hostname": req.Hostname, "from": origin})
		}
		http.Error(w, "this machine is known to the console; an administrator must allow it to enrol", http.StatusConflict)
		return
	}
	if err != nil {
		s.fail(w, r, "enrol", err)
		return
	}
	if s.Events != nil {
		s.Events.Ops("info", "device_api", "enrolled "+req.Hostname, map[string]any{"hostname": req.Hostname, "from": origin, "created": created, "reenrolled": reenrolled})
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
