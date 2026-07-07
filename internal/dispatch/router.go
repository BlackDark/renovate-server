package dispatch

import (
	"fmt"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/BlackDark/renovate-server/internal/config"
	"github.com/BlackDark/renovate-server/internal/executor"
)

// Route is the outcome of matching a repo against the rules.
type Route struct {
	Executor executor.Executor // nil iff Disabled
	Disabled bool
}

type compiledRule struct {
	pattern  string
	disabled bool
	exec     executor.Executor
}

// Router matches repo full names against ordered rules, first match wins.
// Config validation guarantees a catch-all rule exists.
type Router struct {
	rules []compiledRule
}

func NewRouter(rules []config.Rule, executors map[string]executor.Executor) (*Router, error) {
	r := &Router{}
	for i, rule := range rules {
		cr := compiledRule{pattern: rule.Match, disabled: rule.Disabled}
		if !rule.Disabled {
			exec, ok := executors[rule.Executor]
			if !ok {
				return nil, fmt.Errorf("rule %d: unknown executor %q", i, rule.Executor)
			}
			cr.exec = exec
		}
		r.rules = append(r.rules, cr)
	}
	return r, nil
}

func (r *Router) Route(repoFullName string) Route {
	for _, rule := range r.rules {
		ok, err := doublestar.Match(rule.pattern, repoFullName)
		if err != nil || !ok {
			continue
		}
		return Route{Executor: rule.exec, Disabled: rule.disabled}
	}
	// Unreachable with a validated config (catch-all required); treat as disabled.
	return Route{Disabled: true}
}
