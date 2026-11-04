package adminweb

import (
	"strings"
	"testing"
)

func TestLogFilterKeepsToTheTeam(t *testing.T) {
	cases := []struct {
		f    logFilter
		want string
	}{
		{logFilter{}, "event_type: llm_call"},
		{logFilter{Employee: "peter"}, `event_type: llm_call and employee_id: "emp-peter"`},
		// Before the move the same person was logged without the team.
		{logFilter{Employee: "peter", Team: "acme"},
			`event_type: llm_call and (employee_id: "acme-emp-peter" or employee_id: "emp-peter")`},
		// Other customers' calls share the logstore; only ours are listed.
		{logFilter{Team: "acme"}, `event_type: llm_call and (employee_id: acme-emp-* or employee_id: emp-*)`},
	}
	for _, c := range cases {
		if got := c.f.filterQuery(); got != c.want {
			t.Errorf("%+v:\n got %s\nwant %s", c.f, got, c.want)
		}
	}
}

func TestATeamNeedsDatabaseMode(t *testing.T) {
	_, err := New(Options{Listen: "127.0.0.1:0", GatewayTeamID: "acme"})
	if err == nil || !strings.Contains(err.Error(), "database mode") {
		t.Fatalf("err = %v", err)
	}
	if _, err := New(Options{Listen: "127.0.0.1:0", GatewayTeamID: "Acme Corp"}); err == nil {
		t.Fatal("an invalid team id was accepted")
	}
}
