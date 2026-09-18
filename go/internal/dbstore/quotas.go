package dbstore

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type quotaRepo struct{ q querier }

// quotaColumns casts the money to text on the way out. Scanning numeric into a
// float would undo the point of storing it as numeric: 0.1 does not exist in
// binary floating point, and a budget is compared and summed.
const quotaColumns = `employee_id, monthly_budget::text, currency, rpm, tpm, parallel,
	period_rule, period_tz, version, updated_at`

// A plain decimal, nothing else: no exponents, no signs, no separators. The
// value goes into numeric(12,6), and a string like "1e3" or "50 USD" should be
// refused here rather than become a surprise in the database.
var decimalPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,5})(\.[0-9]{1,6})?$`)

func scanQuota(row scanner) (repo.Quota, error) {
	var q repo.Quota
	var employeeID string
	err := row.Scan(&employeeID, &q.MonthlyBudget, &q.Currency, &q.RPM, &q.TPM, &q.Parallel,
		&q.PeriodRule, &q.PeriodTZ, &q.Version, &q.UpdatedAt)
	if err != nil {
		return repo.Quota{}, err
	}
	return q, nil
}

func (r quotaRepo) Get(ctx context.Context, employeeID string) (repo.Quota, error) {
	q, err := scanQuota(r.q.QueryRow(ctx,
		`select `+quotaColumns+` from employee_quotas where employee_id = $1`, employeeID))
	if err != nil {
		return repo.Quota{}, mapError(err, "read quota")
	}
	return q, nil
}

func (r quotaRepo) Set(ctx context.Context, employeeID string, q repo.Quota, expectVersion int) (repo.Quota, error) {
	q = withQuotaDefaults(q)
	if err := validateQuota(q); err != nil {
		return repo.Quota{}, fmt.Errorf("set quota: %w", err)
	}

	// expectVersion 0 means "this employee has no quota yet". Saying so, and
	// being wrong, is a conflict: something was there, and overwriting it
	// would discard limits somebody set on purpose.
	if expectVersion == 0 {
		stored, err := scanQuota(r.q.QueryRow(ctx,
			`insert into employee_quotas
			   (employee_id, monthly_budget, currency, rpm, tpm, parallel, period_rule, period_tz)
			 values ($1, $2::numeric, $3, $4, $5, $6, $7, $8)
			 returning `+quotaColumns,
			employeeID, q.MonthlyBudget, q.Currency, q.RPM, q.TPM, q.Parallel, q.PeriodRule, q.PeriodTZ))
		if err != nil {
			if isUniqueViolation(err) {
				return repo.Quota{}, fmt.Errorf("set quota: %w (a quota already exists)", repo.ErrConflict)
			}
			return repo.Quota{}, mapError(err, "set quota")
		}
		return stored, nil
	}

	stored, err := scanQuota(r.q.QueryRow(ctx,
		`update employee_quotas
		    set monthly_budget = $3::numeric, currency = $4, rpm = $5, tpm = $6, parallel = $7,
		        period_rule = $8, period_tz = $9, version = version + 1, updated_at = now()
		  where employee_id = $1 and version = $2
		  returning `+quotaColumns,
		employeeID, expectVersion, q.MonthlyBudget, q.Currency, q.RPM, q.TPM, q.Parallel,
		q.PeriodRule, q.PeriodTZ))
	if err == nil {
		return stored, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		current, lookupErr := r.Get(ctx, employeeID)
		if lookupErr != nil {
			return repo.Quota{}, fmt.Errorf("set quota: %w", lookupErr)
		}
		return repo.Quota{}, fmt.Errorf("set quota: %w (read version %d, now %d)",
			repo.ErrConflict, expectVersion, current.Version)
	}
	return repo.Quota{}, mapError(err, "set quota")
}

// withQuotaDefaults fills the fields a caller has no opinion about. They are
// filled here rather than in the database so that what Set returns is what was
// stored, without a second read.
func withQuotaDefaults(q repo.Quota) repo.Quota {
	q.MonthlyBudget = strings.TrimSpace(q.MonthlyBudget)
	if q.Currency == "" {
		q.Currency = repo.DefaultCurrency
	}
	if q.PeriodRule == "" {
		q.PeriodRule = repo.PeriodCalendarMonthUTC
	}
	if q.PeriodTZ == "" {
		q.PeriodTZ = repo.DefaultPeriodTZ
	}
	return q
}

func validateQuota(q repo.Quota) error {
	if !decimalPattern.MatchString(q.MonthlyBudget) {
		return fmt.Errorf("monthly budget %q is not a plain decimal amount", q.MonthlyBudget)
	}
	if strings.Trim(q.MonthlyBudget, "0.") == "" {
		// The gateway treats a zero budget as "blocked immediately", which is
		// never what somebody filling in a form meant.
		return errors.New("monthly budget must be greater than zero")
	}
	switch {
	case q.RPM <= 0:
		return errors.New("requests per minute must be greater than zero")
	case q.TPM <= 0:
		return errors.New("tokens per minute must be greater than zero")
	case q.Parallel <= 0:
		return errors.New("parallel requests must be greater than zero")
	case q.PeriodRule != repo.PeriodCalendarMonthUTC:
		// The gateway only resets on the calendar month; storing a rule it
		// does not implement would be a promise the console cannot keep.
		return fmt.Errorf("period rule %q is not supported", q.PeriodRule)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	return errors.Is(mapError(err, "x"), repo.ErrDuplicate)
}
