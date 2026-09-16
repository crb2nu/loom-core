package policy

import (
	"fmt"
	"strings"

	"github.com/crb2nu/loom/internal/loomconcurrency"
)

// ConcurrencyPolicyValidationError describes an invalid pipeline concurrency
// policy. Callers can use errors.As to distinguish rejected configuration from
// runtime failures.
type ConcurrencyPolicyValidationError struct {
	Fields []string
	Value  int
	Min    int
	Max    int
	Cause  error
}

func (e *ConcurrencyPolicyValidationError) Error() string {
	if len(e.Fields) > 1 {
		return fmt.Sprintf("conflicting pipeline concurrency policy fields %s", strings.Join(e.Fields, ", "))
	}
	if len(e.Fields) == 1 {
		return fmt.Sprintf("%s %d is outside allowed range [%d, %d]: %v", e.Fields[0], e.Value, e.Min, e.Max, e.Cause)
	}
	return fmt.Sprintf("invalid pipeline concurrency policy: %v", e.Cause)
}

// Unwrap exposes the bounds-validation cause when one is present.
func (e *ConcurrencyPolicyValidationError) Unwrap() error { return e.Cause }

type concurrencyPolicyField struct {
	name  string
	value *int
}

// Validate rejects conflicting spellings and explicit values outside the
// supported concurrency range.
func (p PipelineConcurrencyPolicy) Validate() error {
	fields := []concurrencyPolicyField{
		{name: "max_concurrency", value: p.MaxConcurrency},
		{name: "max_concurrent_pipelines", value: p.MaxConcurrentPipelines},
		{name: "concurrency_limit", value: p.Limit},
	}

	var configured []concurrencyPolicyField
	for _, field := range fields {
		if field.value != nil {
			configured = append(configured, field)
		}
	}
	if len(configured) == 0 {
		return nil
	}

	value := *configured[0].value
	for _, field := range configured[1:] {
		if *field.value != value {
			names := make([]string, 0, len(configured))
			for _, configuredField := range configured {
				names = append(names, configuredField.name)
			}
			return &ConcurrencyPolicyValidationError{
				Fields: names,
				Cause:  fmt.Errorf("configured values disagree"),
			}
		}
	}

	if err := loomconcurrency.Validate(value); err != nil {
		return &ConcurrencyPolicyValidationError{
			Fields: []string{configured[0].name},
			Value:  value,
			Min:    MinConcurrency,
			Max:    MaxConcurrency,
			Cause:  err,
		}
	}
	return nil
}
