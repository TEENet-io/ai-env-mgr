package model

// ManagedSkills is an authoritative, machine-specific snapshot. An absent
// snapshot means the server does not support skills; an empty list revokes.
type ManagedSkills struct {
	User   string         `json:"user"`
	Skills []ManagedSkill `json:"skills"`
}

type ManagedSkill struct {
	Name    string            `json:"name"`
	Version int               `json:"version"`
	SHA256  string            `json:"sha256"`
	Files   map[string][]byte `json:"files"`
}

type ManagedSkillVersion struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
	SHA256  string `json:"sha256"`
}

type ManagedSkillsStatus struct {
	State          string                `json:"state"`
	User           string                `json:"user,omitempty"`
	Skills         []ManagedSkillVersion `json:"skills"`
	RestartPending []string              `json:"restartPending,omitempty"`
}
