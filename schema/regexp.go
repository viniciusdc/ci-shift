package schema

import (
	"regexp"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// lenientRegexp compiles ECMA-262 patterns with Go's RE2 engine. Patterns
// RE2 cannot express (lookarounds, backreferences) are accepted and match
// everything, so one exotic pattern does not make a whole schema unusable.
func lenientRegexp(s string) (jsonschema.Regexp, error) {
	re, err := regexp.Compile(s)
	if err != nil {
		return anything(s), nil
	}
	return re, nil
}

type anything string

func (a anything) String() string        { return string(a) }
func (anything) MatchString(string) bool { return true }
