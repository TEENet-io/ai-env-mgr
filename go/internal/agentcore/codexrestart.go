package agentcore

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// codexRestartMarkerFile remembers the last one-shot restart this machine
// carried out. It lives beside the other markers in the state directory.
const codexRestartMarkerFile = "codex-restart-nonce"

// noBoundUserProfileNote explains a request that could not be carried out
// because there is nobody on this machine to carry it out for.
const noBoundUserProfileNote = "no bound user profile"

// codexRestartMark is what the agent remembers about the console's last
// one-shot restart request.
//
// It stores the outcome as well as the nonce because the agent acts once and
// then keeps reading the same binding forever after. Without the recorded
// time and note, the very next cycle would report an empty outcome for a
// request it had just carried out, and the console would show "still
// pending" for a restart that already happened.
type codexRestartMark struct {
	Nonce string `json:"nonce"`
	At    string `json:"at"`   // RFC3339
	Note  string `json:"note"` // what came of it
}

func (s *Syncer) readCodexRestartMark() codexRestartMark {
	raw := strings.TrimSpace(s.readMarker(codexRestartMarkerFile))
	if raw == "" {
		return codexRestartMark{}
	}
	var m codexRestartMark
	if err := json.Unmarshal([]byte(raw), &m); err != nil || m.Nonce == "" {
		// Not JSON: treat whatever is there as the nonce alone. The outcome
		// is lost, but the guarantee that matters -- one kill per nonce --
		// still holds, which is the right way round to fail.
		return codexRestartMark{Nonce: raw}
	}
	return m
}

func (s *Syncer) writeCodexRestartMark(m codexRestartMark) {
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	s.writeMarker(codexRestartMarkerFile, string(data))
}

// runCodexRestart carries out the console's one-shot request, at most once
// per nonce, and reports what to put in this machine's status.
//
// The marker is written even when the kill fails. A request that cannot
// succeed on this machine -- taskkill denied, a filter that matches nothing
// it can touch -- would otherwise be retried every cycle forever, and each
// retry is another attempt to take somebody's session away. The failure is
// recorded in the note instead, where an administrator can see it and decide
// whether to ask again.
func (s *Syncer) runCodexRestart(b model.Binding, userExists bool, warns *[]string) codexRestartMark {
	if b.RestartCodex == "" {
		return codexRestartMark{}
	}
	if prev := s.readCodexRestartMark(); prev.Nonce == b.RestartCodex {
		// Already done. Report the recorded outcome again rather than acting:
		// the console reads "has this been carried out?" from the status.
		return prev
	}
	if !userExists {
		// Left unmarked on purpose: the employee's profile may simply not
		// exist yet (a machine bound before its first sign-in), and the
		// request should still run once it does. Reported as pending, with
		// the reason attached.
		*warns = append(*warns, fmt.Sprintf("codex restart requested for %q but %s", b.User, noBoundUserProfileNote))
		return codexRestartMark{Note: noBoundUserProfileNote}
	}

	m := codexRestartMark{Nonce: b.RestartCodex, At: time.Now().UTC().Format(time.RFC3339)}
	killed, err := s.Applier.StopCodex(b.User)
	switch {
	case err != nil:
		m.Note = err.Error()
		*warns = append(*warns, fmt.Sprintf("codex restart for %q as requested FAILED: %v", b.User, err))
	default:
		m.Note = killedNote(killed)
		*warns = append(*warns, fmt.Sprintf("codex restarted for %q as requested (%s)", b.User, m.Note))
	}
	s.writeCodexRestartMark(m)
	return m
}

// killedNote says what a kill did, in the few words the console has room for.
func killedNote(killed int) string {
	switch killed {
	case 0:
		// Not a failure: the employee did not have Codex open. Saying so
		// beats an empty cell that reads as "nothing happened, unclear why".
		return "no process"
	case 1:
		return "killed 1 process"
	default:
		return fmt.Sprintf("killed %d processes", killed)
	}
}
