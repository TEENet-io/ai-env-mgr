package companyskills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

const SettingKey = "company_skills_v1"
const MaxCatalogBytes = 32 << 20
const MaxPlanBytes = 8 << 20

type Revision struct {
	Bundle model.ManagedSkill `json:"bundle"`
	By     string             `json:"by"`
	At     time.Time          `json:"at"`
}

type Skill struct {
	Name          string     `json:"name"`
	Title         string     `json:"title"`
	Revisions     []Revision `json:"revisions"`
	Current       int        `json:"current"`
	TrialEmployee string     `json:"trialEmployee,omitempty"`
	Employees     []string   `json:"employees"`
	Departments   []string   `json:"departments"`
}

func (s Skill) Bundle() (model.ManagedSkill, error) {
	for _, rev := range s.Revisions {
		if rev.Bundle.Version == s.Current {
			return rev.Bundle, Validate(rev.Bundle)
		}
	}
	return model.ManagedSkill{}, errors.New("当前 skill 版本不存在")
}

type Catalog struct {
	Skills []Skill `json:"skills"`
}

func Load(ctx context.Context, settings repo.Settings) (Catalog, int, error) {
	row, err := settings.Get(ctx, SettingKey)
	if errors.Is(err, repo.ErrNotFound) {
		return Catalog{Skills: []Skill{}}, 0, nil
	}
	if err != nil {
		return Catalog{}, 0, err
	}
	var c Catalog
	if err := json.Unmarshal(row.Value, &c); err != nil {
		return c, 0, fmt.Errorf("读取公司 skill 清单：%w", err)
	}
	if c.Skills == nil || len(c.Skills) > MaxSkills {
		return c, 0, errors.New("公司 skill 清单不完整")
	}
	seen := map[string]bool{}
	for _, skill := range c.Skills {
		if !ValidName(skill.Name) || seen[skill.Name] {
			return c, 0, errors.New("公司 skill 清单名称无效或重复")
		}
		seen[skill.Name] = true
		bundle, err := skill.Bundle()
		if err != nil {
			return c, 0, err
		}
		if bundle.Name != skill.Name {
			return c, 0, errors.New("公司 skill 版本名称不一致")
		}
	}
	return c, row.Version, nil
}

// Desired resolves audiences against the live roster, including department
// changes. Only this employee's bundles cross the device API.
func (c Catalog) Desired(employee repo.Employee) (*model.ManagedSkills, error) {
	plan := &model.ManagedSkills{Skills: []model.ManagedSkill{}}
	if !employee.Active() || employee.Deleted() {
		return plan, nil
	}
	plan.User = employee.WindowsUser
	total := 0
	for _, skill := range c.Skills {
		matches := skill.TrialEmployee == employee.ID
		for _, id := range skill.Employees {
			matches = matches || id == employee.ID
		}
		for _, department := range skill.Departments {
			matches = matches || department != "" && employee.Department == department
		}
		if !matches {
			continue
		}
		bundle, err := skill.Bundle()
		if err != nil {
			return nil, err
		}
		for _, data := range bundle.Files {
			total += len(data)
		}
		if total > MaxPlanBytes {
			return nil, errors.New("员工 skill 总量超过 8 MiB，请减少附件")
		}
		plan.Skills = append(plan.Skills, bundle)
	}
	sort.Slice(plan.Skills, func(i, j int) bool { return plan.Skills[i].Name < plan.Skills[j].Name })
	return plan, nil
}

type Change struct {
	Action                 string
	Name, Title            string
	Bundle                 model.ManagedSkill
	TrialEmployee          string
	Employees, Departments []string
	Revision               int
	ExpectedVersion        int
	ActorType, ActorID     string
	ActorName              string
}

// Apply commits the catalog and its audit event in the same transaction. The
// existing settings repository supplies CAS and PostgreSQL durability; no OSS
// credentials, installer rebuild, or separate distribution service is needed.
func Apply(ctx context.Context, store repo.Store, change Change) error {
	if change.ActorID == "" || (change.ActorType != "admin" && change.ActorType != "customer") {
		return errors.New("缺少操作身份")
	}
	return store.InTx(ctx, func(tx repo.Store) error {
		if err := tx.Lock(ctx, SettingKey); err != nil {
			return err
		}
		catalog, version, err := Load(ctx, tx.Settings())
		if err != nil {
			return err
		}
		if version != change.ExpectedVersion {
			return repo.ErrConflict
		}
		index := -1
		for i := range catalog.Skills {
			if catalog.Skills[i].Name == change.Name {
				index = i
				break
			}
		}
		var before Skill
		if index >= 0 {
			before = catalog.Skills[index]
		}
		skill := before
		switch change.Action {
		case "save":
			if change.Bundle.Name != change.Name || strings.TrimSpace(change.Title) == "" || len(change.Title) > 200 {
				return errors.New("请填写 skill 名称和业务名称")
			}
			change.Bundle.Version = len(skill.Revisions) + 1
			if err := Validate(change.Bundle); err != nil {
				return err
			}
			if len(skill.Revisions) >= 64 {
				return errors.New("每个 skill 最多保留 64 个版本，请另建 skill")
			}
			if index < 0 {
				if len(catalog.Skills) >= MaxSkills {
					return errors.New("公司 skill 数量已达 64 个")
				}
				skill.Name = change.Name
				skill.TrialEmployee = change.TrialEmployee
			}
			skill.Title = strings.TrimSpace(change.Title)
			skill.Current = change.Bundle.Version
			skill.Revisions = append(append([]Revision{}, skill.Revisions...), Revision{Bundle: change.Bundle, By: change.ActorID, At: time.Now().UTC()})
		case "assign":
			if index < 0 {
				return repo.ErrNotFound
			}
			skill.Employees, skill.Departments = unique(change.Employees), unique(change.Departments)
			skill.TrialEmployee = ""
		case "trial":
			if index < 0 || change.TrialEmployee == "" {
				return errors.New("请选择试用员工")
			}
			if len(skill.Employees) > 0 || len(skill.Departments) > 0 {
				return errors.New("已派发的 skill 请先撤回再试用")
			}
			skill.TrialEmployee = change.TrialEmployee
		case "rollback":
			if index < 0 {
				return repo.ErrNotFound
			}
			skill.Current = change.Revision
			if _, err := skill.Bundle(); err != nil {
				return err
			}
		case "revoke":
			if index < 0 {
				return repo.ErrNotFound
			}
			skill.Employees, skill.Departments, skill.TrialEmployee = nil, nil, ""
		default:
			return errors.New("未知 skill 操作")
		}
		// Validate selectors against the current roster, without reviving an
		// offboarded account or guessing a contact's identity from their email.
		roster, err := tx.Employees().List(ctx, repo.EmployeeFilter{})
		if err != nil {
			return err
		}
		ids, departments := map[string]bool{}, map[string]bool{}
		for _, e := range roster {
			if e.Active() && !e.Deleted() {
				ids[e.ID] = true
				if e.Department != "" {
					departments[e.Department] = true
				}
			}
		}
		if change.Action == "assign" || change.Action == "trial" || change.Action == "save" && index < 0 {
			for _, id := range append(append([]string{}, skill.Employees...), skill.TrialEmployee) {
				if id != "" && !ids[id] {
					return errors.New("派发对象须为在职员工")
				}
			}
			for _, department := range skill.Departments {
				if !departments[department] {
					return errors.New("派发部门不存在")
				}
			}
		}
		if index < 0 {
			catalog.Skills = append(catalog.Skills, skill)
		} else {
			catalog.Skills[index] = skill
		}
		for _, e := range roster {
			if _, err := catalog.Desired(e); err != nil {
				return err
			}
		}
		data, err := json.Marshal(catalog)
		if err != nil {
			return err
		}
		if len(data) > MaxCatalogBytes {
			return errors.New("公司 skill 清单和历史总量超过 32 MiB")
		}
		if _, err := tx.Settings().Set(ctx, SettingKey, data, version, change.ActorID); err != nil {
			return err
		}
		// Audit metadata and audiences, rather than copying customer templates
		// and business instructions into the general audit archive.
		summary := func(s Skill) []byte {
			b, _ := json.Marshal(struct {
				Current                int    `json:"version"`
				Trial                  string `json:"trial"`
				Employees, Departments []string
			}{s.Current, s.TrialEmployee, s.Employees, s.Departments})
			return b
		}
		detail, _ := json.Marshal(map[string]string{"actor_name": change.ActorName, "title": skill.Title})
		_, err = tx.Audit().Append(ctx, repo.AuditEvent{ActorType: change.ActorType, ActorID: change.ActorID, Action: "skill." + change.Action, TargetType: "company_skill", TargetID: change.Name, Before: summary(before), After: summary(skill), Detail: detail, Result: "ok"})
		return err
	})
}

func unique(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}
