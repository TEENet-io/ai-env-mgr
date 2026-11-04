package litellm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Customers share one gateway and are told apart by a LiteLLM team: every
// key this console mints carries the team id, every employee is a member,
// and every list this console reads is narrowed to it. A customer with
// channels of its own still gets a gateway of its own, and runs with no team.

// teamIDPattern keeps team ids to lower-case letters, digits and underscores.
// No hyphen, because the id is the first part of every gateway user id
// ("<team>-emp-<user>"), and a team named "a-emp-b" could otherwise collide
// with team "a"'s employee "b-...".
var teamIDPattern = regexp.MustCompile(`^[a-z0-9_]{2,32}$`)

// ValidateTeamID reports why id cannot be a team id. Empty is valid: it is a
// gateway this customer does not share.
func ValidateTeamID(id string) error {
	if id == "" || teamIDPattern.MatchString(id) {
		return nil
	}
	return fmt.Errorf("team id %q: use 2-32 lower-case letters, digits or underscores", id)
}

// EmployeeUserID is the gateway user id for an employee of team. It is
// derived from the Windows user name and deliberately does not carry the
// epoch: the user owns the spend history, and minting a new one on every
// re-issue would scatter one person's usage across a row of accounts.
//
// The team prefix is what lets two customers each have a "peter": without
// it the second one's onboarding would overwrite the first one's budget and
// the second one's deletion would delete the first one.
func EmployeeUserID(team, windowsUser string) string {
	id := "emp-" + strings.ToLower(windowsUser)
	if team == "" {
		return id
	}
	return team + "-" + id
}

// EmployeeKeyAlias is the gateway's name for one issued token. Unlike the
// user id it carries the epoch, so that a re-issue is a new alias rather
// than a collision, and a fragment of the employee's own id, so that a
// deleted account's name reused by a new person starts its aliases afresh
// instead of running into the old account's history. Aliases are unique
// across the whole gateway, so they carry the team too.
func EmployeeKeyAlias(team, windowsUser, employeeID string, epoch int) string {
	id := strings.ReplaceAll(employeeID, "-", "")
	if len(id) > 8 {
		id = id[:8]
	}
	return fmt.Sprintf("%s-%s-e%d", EmployeeUserID(team, windowsUser), id, epoch)
}

// EnsureTeam creates the client's team on the gateway if it is not there.
// Keys can be minted for a team that does not exist, but members cannot be
// added to one, and a missing team answers 404 -- which the provisioning
// queue reads as "already gone" and would report as success.
func (c *Client) EnsureTeam(ctx context.Context) error {
	if c.TeamID == "" {
		return nil
	}
	err := c.do(ctx, http.MethodGet, "/team/info?team_id="+url.QueryEscape(c.TeamID), nil, nil)
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		return fmt.Errorf("look up team %q: %w", c.TeamID, err)
	}
	if err := c.do(ctx, http.MethodPost, "/team/new", map[string]any{
		"team_id":    c.TeamID,
		"team_alias": c.TeamID,
	}, nil); err != nil {
		return fmt.Errorf("create team %q: %w", c.TeamID, err)
	}
	return nil
}

// addTeamMember makes userID a member of the client's team. A key can only
// be minted for a user in the key's team ("User=... not assigned to team"),
// so this runs on every upsert; a user who is already a member is the
// outcome wanted.
func (c *Client) addTeamMember(ctx context.Context, userID string) error {
	err := c.do(ctx, http.MethodPost, "/team/member_add", map[string]any{
		"team_id": c.TeamID,
		"member":  map[string]string{"role": "user", "user_id": userID},
	}, nil)
	if isAlreadyMember(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("add %q to team %q: %w", userID, c.TeamID, err)
	}
	return nil
}

func isAlreadyMember(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		return false
	}
	return strings.Contains(apiErr.Body, "team_member_already_in_team") || strings.Contains(apiErr.Body, "already in team")
}
