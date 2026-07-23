// Package health exposes secret-free liveness and readiness contracts.
package health

import (
	"context"
	"errors"
	"strings"
	"time"
)

type Status string

const (
	StatusOK       Status = "ok"
	StatusNotReady Status = "not_ready"
	defaultTimeout        = 5 * time.Second
)

type CheckResult struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type Report struct {
	Status Status        `json:"status"`
	At     time.Time     `json:"at"`
	Checks []CheckResult `json:"checks,omitempty"`
}

type Check func(context.Context) error

type Service struct {
	clock   func() time.Time
	timeout time.Duration
	checks  []namedCheck
}

type namedCheck struct {
	name  string
	check Check
}

func New(clock func() time.Time, checks map[string]Check) (*Service, error) {
	return NewWithTimeout(clock, checks, defaultTimeout)
}

func NewWithTimeout(clock func() time.Time, checks map[string]Check, timeout time.Duration) (*Service, error) {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	if timeout <= 0 {
		return nil, errors.New("health: invalid timeout")
	}
	values := make([]namedCheck, 0, len(checks))
	for name, check := range checks {
		if strings.TrimSpace(name) == "" || check == nil {
			return nil, errors.New("health: invalid check")
		}
		values = append(values, namedCheck{name: name, check: check})
	}
	for i := 0; i < len(values); i++ {
		for j := i + 1; j < len(values); j++ {
			if values[j].name < values[i].name {
				values[i], values[j] = values[j], values[i]
			}
		}
	}
	return &Service{clock: clock, timeout: timeout, checks: values}, nil
}

func Liveness(clock func() time.Time) Report {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return Report{Status: StatusOK, At: clock().UTC()}
}

func (s *Service) Readiness(ctx context.Context) Report {
	if s == nil {
		return Report{Status: StatusNotReady, At: time.Now().UTC(), Checks: []CheckResult{{Name: "service", Status: StatusNotReady, Detail: "unavailable"}}}
	}
	checkCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	report := Report{Status: StatusOK, At: s.clock().UTC(), Checks: make([]CheckResult, 0, len(s.checks))}
	for _, item := range s.checks {
		result := CheckResult{Name: item.name, Status: StatusOK}
		if err := item.check(checkCtx); err != nil {
			result.Status = StatusNotReady
			result.Detail = safeDetail(err)
			report.Status = StatusNotReady
		}
		report.Checks = append(report.Checks, result)
	}
	return report
}

func safeDetail(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return "unavailable"
}
