// Package directory reads the customer's list of people from their custom
// tool and works out what has changed since the console last looked.
//
// The custom tool is where a customer keeps its accounts (AIC-206). The
// console copies that list every few minutes and acts on the differences:
// a new person can raise tickets, a renamed one is shown under the new name,
// a disabled one loses every session. Taking access away is the dangerous
// direction, so the rules here lean towards doing nothing: only an explicit
// "disabled" or "deleted" counts, a person missing from a reply is left as
// they were, and more than a handful at once waits for a person to confirm.
package directory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Roles and statuses as the custom tool sends them.
const (
	RoleOps      = "ops"
	RoleAdmin    = "admin"
	RoleEmployee = "employee"

	// What a customer admin may do in the ticket system. The tool sends it
	// with admins only; an admin without one is approving, as every admin
	// was before the tool could say otherwise.
	TicketApprove = "approve"
	TicketView    = "view"

	StatusActive   = "active"
	StatusDisabled = "disabled"
	StatusDeleted  = "deleted"
)

// Account is one person on the list.
type Account struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Role     string `json:"role"`
	// TicketRole is TicketApprove or TicketView for an admin, "" otherwise.
	TicketRole string `json:"ticket_role,omitempty"`
	Status     string `json:"status"`
}

// TicketRoleFor is the ticket role an account is taken to have: what the
// tool sent for an admin, approving when it sent nothing, and none for the
// other roles whatever was sent.
func TicketRoleFor(role, sent string) (string, error) {
	if role != RoleAdmin {
		return "", nil
	}
	switch sent {
	case "":
		return TicketApprove, nil
	case TicketApprove, TicketView:
		return sent, nil
	}
	return "", fmt.Errorf("unknown ticket role %q", sent)
}

// Active reports whether the account may sign in.
func (a Account) Active() bool { return a.Status == StatusActive }

// DisplayName is the name to show: the display name, or the username when
// the tool has none.
func (a Account) DisplayName() string {
	if n := strings.TrimSpace(a.Name); n != "" {
		return n
	}
	return a.Username
}

// Gone is a live AI workspace account whose person the list has as
// disabled or deleted: one the sync closes when it is allowed to.
type Gone struct {
	EmployeeID, WindowsUser string
	Person                  Account
}

// List is one reply from the custom tool.
type List struct {
	GeneratedAt time.Time `json:"generated_at"`
	Accounts    []Account `json:"accounts"`
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ErrEmpty is a reply with nobody in it. A customer always has at least its
// own administrator, so an empty list is a fault on the other side, and
// acting on it would take everybody's access away.
var ErrEmpty = errors.New("the directory returned no accounts")

// Validate refuses a reply that cannot be trusted as a whole: one bad row
// means the format is not what the console expects, and acting on the rest
// would be guessing. Ids are lower-cased so they compare as stored.
func (l *List) Validate() error {
	if len(l.Accounts) == 0 {
		return ErrEmpty
	}
	seen := make(map[string]bool, len(l.Accounts))
	for i := range l.Accounts {
		a := &l.Accounts[i]
		a.ID = strings.ToLower(strings.TrimSpace(a.ID))
		a.Email = strings.ToLower(strings.TrimSpace(a.Email))
		a.Username = strings.TrimSpace(a.Username)
		if !uuidPattern.MatchString(a.ID) {
			return fmt.Errorf("account %d: id %q is not a UUID", i, a.ID)
		}
		if seen[a.ID] {
			return fmt.Errorf("account %s appears twice", a.ID)
		}
		seen[a.ID] = true
		if a.Username == "" {
			return fmt.Errorf("account %s has no username", a.ID)
		}
		switch a.Role {
		case RoleOps, RoleAdmin, RoleEmployee:
		default:
			return fmt.Errorf("account %s has unknown role %q", a.ID, a.Role)
		}
		ticketRole, err := TicketRoleFor(a.Role, a.TicketRole)
		if err != nil {
			return fmt.Errorf("account %s: %w", a.ID, err)
		}
		a.TicketRole = ticketRole
		switch a.Status {
		case StatusActive, StatusDisabled, StatusDeleted:
		default:
			return fmt.Errorf("account %s has unknown status %q", a.ID, a.Status)
		}
	}
	return nil
}

// Client reads the list over HTTP with the token the custom tool issued to
// this console.
type Client struct {
	URL   string // the full address of GET /internal/directory/accounts
	Token string
	HTTP  *http.Client
}

// maxReply bounds what is read: a customer's staff list is kilobytes, and a
// reply of many megabytes is a fault, not a large company.
const maxReply = 8 << 20

// Fetch reads and validates the whole list.
func (c Client) Fetch(ctx context.Context) (List, error) {
	if c.URL == "" || c.Token == "" {
		return List{}, errors.New("directory: address or token not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return List{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	res, err := hc.Do(req)
	if err != nil {
		return List{}, fmt.Errorf("directory: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxReply+1))
	if err != nil {
		return List{}, fmt.Errorf("directory: read reply: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return List{}, fmt.Errorf("directory: HTTP %d", res.StatusCode)
	}
	if len(body) > maxReply {
		return List{}, errors.New("directory: reply too large")
	}
	var list List
	if err := json.Unmarshal(body, &list); err != nil {
		return List{}, fmt.Errorf("directory: decode reply: %w", err)
	}
	if err := list.Validate(); err != nil {
		return List{}, fmt.Errorf("directory: %w", err)
	}
	return list, nil
}
