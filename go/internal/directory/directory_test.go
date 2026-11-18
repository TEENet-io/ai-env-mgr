package directory

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func account(n int, role, status string) Account {
	return Account{
		ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", n), Username: fmt.Sprintf("u%d@example.com", n),
		Name: fmt.Sprintf("User %d", n), Email: fmt.Sprintf("u%d@example.com", n), Role: role, Status: status,
	}
}

func known(accounts ...Account) map[string]Account {
	m := map[string]Account{}
	for _, a := range accounts {
		m[a.ID] = a
	}
	return m
}

func kinds(cs []Change) []string {
	out := []string{}
	for _, c := range cs {
		out = append(out, c.Kind+":"+c.After.Username)
	}
	return out
}

func TestValidateRefusesARepliesItCannotTrust(t *testing.T) {
	good := account(1, RoleAdmin, StatusActive)
	for name, list := range map[string]List{
		"empty":        {},
		"bad id":       {Accounts: []Account{{ID: "7", Username: "a", Role: RoleAdmin, Status: StatusActive}}},
		"twice":        {Accounts: []Account{good, good}},
		"no username":  {Accounts: []Account{{ID: good.ID, Role: RoleAdmin, Status: StatusActive}}},
		"odd role":     {Accounts: []Account{{ID: good.ID, Username: "a", Role: "root", Status: StatusActive}}},
		"odd status":   {Accounts: []Account{{ID: good.ID, Username: "a", Role: RoleAdmin, Status: "gone"}}},
		"blank status": {Accounts: []Account{{ID: good.ID, Username: "a", Role: RoleAdmin}}},
	} {
		if err := list.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	upper := List{Accounts: []Account{{ID: strings.ToUpper(good.ID), Username: "a", Email: "A@Example.com", Role: RoleAdmin, Status: StatusActive}}}
	if err := upper.Validate(); err != nil || upper.Accounts[0].ID != good.ID || upper.Accounts[0].Email != "a@example.com" {
		t.Errorf("an upper-case id or email was not normalised: %+v %v", upper.Accounts[0], err)
	}
}

func TestCompareFindsEachKindOfChange(t *testing.T) {
	stay, renamed, moved, promoted, leaving, back, purged := account(1, RoleAdmin, StatusActive), account(2, RoleEmployee, StatusActive),
		account(3, RoleEmployee, StatusActive), account(4, RoleEmployee, StatusActive), account(5, RoleEmployee, StatusActive),
		account(6, RoleEmployee, StatusDisabled), account(7, RoleEmployee, StatusDisabled)
	gone := account(8, RoleEmployee, StatusActive)
	now := []Account{stay, renamed, moved, promoted, leaving, back, purged, account(9, RoleEmployee, StatusActive)}
	now[1].Name = "Renamed"
	now[2].Email = "new@example.com"
	now[3].Role = RoleAdmin
	now[4].Status = StatusDeleted
	now[5].Status = StatusActive
	now[6].Status = StatusDeleted

	p := Compare(known(stay, renamed, moved, promoted, leaving, back, purged, gone), List{Accounts: now})
	got := strings.Join(kinds(p.Changes), " ")
	want := "renamed:u2@example.com email:u3@example.com role:u4@example.com reenabled:u6@example.com deleted:u7@example.com added:u9@example.com disabled:u5@example.com"
	if got != want {
		t.Errorf("changes:\n got %s\nwant %s", got, want)
	}
	if len(p.Held) != 0 {
		t.Errorf("one disable was held: %v", kinds(p.Held))
	}
	if len(p.Missing) != 1 || p.Missing[0].ID != gone.ID {
		t.Errorf("missing = %v, want only the one absent from the reply", p.Missing)
	}
}

// Somebody missing from a reply has not left: the reply may be cut short.
func TestAnAbsentAccountIsNotDisabled(t *testing.T) {
	a, b := account(1, RoleAdmin, StatusActive), account(2, RoleEmployee, StatusActive)
	p := Compare(known(a, b), List{Accounts: []Account{a}})
	if len(p.Changes) != 0 || len(p.Missing) != 1 {
		t.Fatalf("changes %v, missing %v", kinds(p.Changes), p.Missing)
	}
	stored := p.Store(List{Accounts: []Account{a}})
	if len(stored) != 1 {
		t.Errorf("stored %d accounts; the absent one must be left to the caller", len(stored))
	}
}

func TestMoreThanThreeDisablesAtOnceWaitForAPerson(t *testing.T) {
	before := []Account{account(0, RoleAdmin, StatusActive)}
	for i := 1; i <= MaxDisablesPerSync+1; i++ {
		before = append(before, account(i, RoleEmployee, StatusActive))
	}
	now := make([]Account, len(before))
	copy(now, before)
	for i := 1; i < len(now); i++ {
		now[i].Status = StatusDisabled
	}
	now[1].Name = "Renamed while leaving"

	list := List{Accounts: now}
	p := Compare(known(before...), list)
	if len(p.Held) != MaxDisablesPerSync+1 {
		t.Fatalf("held %d disables, want all %d", len(p.Held), MaxDisablesPerSync+1)
	}
	if got := strings.Join(kinds(p.Changes), " "); got != "renamed:u1@example.com" {
		t.Errorf("applied %q; only the rename should go through", got)
	}
	for _, a := range p.Store(list) {
		if a.Status != StatusActive {
			t.Errorf("%s stored as %s; a held disable keeps the old status", a.Username, a.Status)
		}
	}

	// Exactly the limit goes through.
	now[len(now)-1].Status = StatusActive
	if p := Compare(known(before...), List{Accounts: now}); len(p.Held) != 0 || len(p.Changes) != MaxDisablesPerSync+1 {
		t.Errorf("at the limit: held %d, changes %v", len(p.Held), kinds(p.Changes))
	}
}

func TestFetchSendsTheTokenAndValidates(t *testing.T) {
	var auth string
	reply := `{"generated_at":"2026-11-07T08:00:00Z","accounts":[{"id":"00000000-0000-4000-8000-000000000001","username":"a","name":"A","email":"a@example.com","role":"admin","status":"active"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()

	list, err := Client{URL: srv.URL, Token: "t0ken"}.Fetch(context.Background())
	if err != nil || len(list.Accounts) != 1 || auth != "Bearer t0ken" {
		t.Fatalf("fetch: %+v %v (auth %q)", list, err, auth)
	}

	reply = `{"accounts":[]}`
	if _, err := (Client{URL: srv.URL, Token: "t"}).Fetch(context.Background()); !errors.Is(err, ErrEmpty) {
		t.Errorf("an empty list was accepted: %v", err)
	}
	if _, err := (Client{URL: srv.URL}).Fetch(context.Background()); err == nil {
		t.Error("fetched without a token")
	}
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer failing.Close()
	if _, err := (Client{URL: failing.URL, Token: "t"}).Fetch(context.Background()); err == nil {
		t.Error("an HTTP 500 was accepted")
	}
}

func TestOnlyAdminsCarryATicketRole(t *testing.T) {
	admin, viewer, staff := account(1, RoleAdmin, StatusActive), account(2, RoleAdmin, StatusActive), account(3, RoleEmployee, StatusActive)
	viewer.TicketRole = TicketView
	staff.TicketRole = TicketView // an employee has none, whatever was sent
	list := List{Accounts: []Account{admin, viewer, staff}}
	if err := list.Validate(); err != nil {
		t.Fatal(err)
	}
	got := []string{list.Accounts[0].TicketRole, list.Accounts[1].TicketRole, list.Accounts[2].TicketRole}
	if want := []string{TicketApprove, TicketView, ""}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ticket roles = %q, want %q (an admin with none approves, as before)", got, want)
	}
	bad := account(4, RoleAdmin, StatusActive)
	bad.TicketRole = "boss"
	if err := (&List{Accounts: []Account{bad}}).Validate(); err == nil {
		t.Error("an unknown ticket role was accepted")
	}
}

func TestATicketRoleChangeIsARoleChangeButNotOnDeletion(t *testing.T) {
	before := account(1, RoleAdmin, StatusActive)
	before.TicketRole = TicketApprove
	now := before
	now.TicketRole = TicketView
	if got := kinds(Compare(known(before), List{Accounts: []Account{now}}).Changes); fmt.Sprint(got) != "[role:u1@example.com]" {
		t.Errorf("approve -> view: %v", got)
	}
	// A deleted account's record says nothing about tickets; it arrives as approve.
	gone := before
	gone.TicketRole, gone.Status = TicketApprove, StatusDeleted
	disabled := now
	disabled.Status = StatusDisabled
	if got := kinds(Compare(known(disabled), List{Accounts: []Account{gone}}).Changes); fmt.Sprint(got) != "[deleted:u1@example.com]" {
		t.Errorf("deleting a viewer: %v", got)
	}
}
