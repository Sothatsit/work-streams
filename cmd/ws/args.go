package main

import (
	"fmt"
	"strconv"
	"strings"
)

// flagSpec declares the flags a subcommand accepts. The parser is
// hand-rolled instead of using the flag package so flags can appear
// after positional arguments (`ws add todo "text" --project x`).
type flagSpec struct {
	strs   []string          // flags taking a value
	bools  []string          // flags taking no value
	repeat []string          // repeatable flags taking a value
	alias  map[string]string // short form -> canonical flag
}

type parsedArgs struct {
	pos     []string
	strs    map[string]string
	lists   map[string][]string
	bools   map[string]bool
	present map[string]bool
}

func (p *parsedArgs) has(flag string) bool {
	return p.present[flag]
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func parseArgs(args []string, spec flagSpec) (*parsedArgs, error) {
	p := &parsedArgs{
		strs:    map[string]string{},
		lists:   map[string][]string{},
		bools:   map[string]bool{},
		present: map[string]bool{},
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" || arg == "--" {
			p.pos = append(p.pos, arg)
			continue
		}
		name := arg
		value := ""
		hasValue := false
		if idx := strings.Index(arg, "="); idx >= 0 {
			name = arg[:idx]
			value = arg[idx+1:]
			hasValue = true
		}
		if canonical, ok := spec.alias[name]; ok {
			name = canonical
		}
		switch {
		case contains(spec.bools, name):
			if hasValue {
				return nil, fmt.Errorf("%s does not take a value", name)
			}
			if p.present[name] {
				return nil, fmt.Errorf("%s given more than once", name)
			}
			p.bools[name] = true
			p.present[name] = true
		case contains(spec.strs, name):
			if !hasValue {
				i++
				if i >= len(args) {
					return nil, fmt.Errorf("%s requires a value", name)
				}
				value = args[i]
			}
			if p.present[name] {
				return nil, fmt.Errorf("%s given more than once", name)
			}
			p.strs[name] = value
			p.present[name] = true
		case contains(spec.repeat, name):
			if !hasValue {
				i++
				if i >= len(args) {
					return nil, fmt.Errorf("%s requires a value", name)
				}
				value = args[i]
			}
			p.lists[name] = append(p.lists[name], value)
			p.present[name] = true
		default:
			return nil, fmt.Errorf("unknown flag: %s", name)
		}
	}
	return p, nil
}

var searchFields = []string{
	"subject", "body", "content", "type", "key", "meta",
}

// shorthandKeys are metadata keys with a convenience flag on add and
// search: --project duck-pond is --meta project=duck-pond.
var shorthandKeys = []string{"project", "jira", "github", "confluence"}

func searchFlagSpec() flagSpec {
	spec := flagSpec{
		strs:  []string{"--limit", "--offset"},
		bools: []string{"--order-by-creation", "--order-by-modified", "--id-only"},
		alias: map[string]string{"-n": "--limit"},
	}
	for _, field := range searchFields {
		spec.repeat = append(spec.repeat, "--"+field, "--no-"+field)
	}
	for _, key := range shorthandKeys {
		spec.repeat = append(spec.repeat, "--"+key, "--no-"+key)
	}
	return spec
}

// shorthandFor reports whether a flag is a metadata-key shorthand
// (--project, --no-jira, ...) and returns its key and whether it
// negates.
func shorthandFor(flag string) (key string, negate bool, ok bool) {
	name := strings.TrimPrefix(flag, "--")
	negate = strings.HasPrefix(name, "no-")
	name = strings.TrimPrefix(name, "no-")
	for _, key := range shorthandKeys {
		if name == key {
			return key, negate, true
		}
	}
	return "", false, false
}

// parseEntryID accepts the canonical e-prefixed form (e123) as well as
// a bare number.
func parseEntryID(arg string) (int64, error) {
	s := strings.TrimPrefix(arg, "e")
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid entry id %q (expected e.g. e123)", arg)
	}
	return id, nil
}

func parseLimit(value string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > 500 {
		return 0, fmt.Errorf("--limit must be an integer from 1 to 500")
	}
	return n, nil
}

func parseOffset(value string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("--offset must be a non-negative integer")
	}
	return n, nil
}
