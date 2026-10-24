package collector

import (
	"errors"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// parseSchedules returns the `on.schedule[].cron` entries of a workflow file.
// A workflow without a schedule trigger returns nil, nil.
//
// The file is decoded generically, so anchors, aliases and `<<` merge keys
// are resolved by the YAML decoder. `on` may be a string (`on: push`), a list
// (`on: [push, schedule]`, which cannot carry crons) or a map. YAML 1.1
// parsers read a bare `on` key as the boolean true; yaml.v3 keeps it a
// string, but a key written as `true:` (or produced by a tool that
// round-tripped the file through YAML 1.1) is accepted as well.
func parseSchedules(content []byte) ([]string, error) {
	var doc any
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, fmt.Errorf("invalid workflow YAML: %w", err)
	}
	if doc == nil {
		return nil, nil
	}
	if !isMap(doc) {
		return nil, errors.New("workflow is not a YAML mapping")
	}
	// Prefer a literal "on" key; a YAML 1.1 `true` key is only the fallback,
	// so a file carrying both resolves deterministically.
	on, ok := lookup(doc, func(k any) bool { return k == "on" })
	if !ok {
		on, _ = lookup(doc, func(k any) bool { return k == true })
	}
	if !isMap(on) {
		return nil, nil
	}
	sched, ok := lookup(on, func(k any) bool { return k == "schedule" })
	if !ok || sched == nil {
		return nil, nil
	}
	list, ok := sched.([]any)
	if !ok {
		return nil, errors.New("on.schedule is not a list")
	}
	var crons []string
	for i, item := range list {
		if !isMap(item) {
			return nil, fmt.Errorf("on.schedule[%d] is not a mapping", i)
		}
		v, _ := lookup(item, func(k any) bool { return k == "cron" })
		c, ok := v.(string)
		if !ok || strings.TrimSpace(c) == "" {
			return nil, fmt.Errorf("on.schedule[%d] has no cron", i)
		}
		crons = append(crons, c)
	}
	return crons, nil
}

func isMap(v any) bool {
	switch v.(type) {
	case map[string]any, map[any]any:
		return true
	}
	return false
}

// lookup returns the value of the first key in a decoded mapping matching
// match. yaml.v3 decodes a mapping to map[string]any when every key is a
// string and to map[any]any otherwise (e.g. a `true:` key).
func lookup(m any, match func(any) bool) (any, bool) {
	switch mm := m.(type) {
	case map[string]any:
		for k, v := range mm {
			if match(k) {
				return v, true
			}
		}
	case map[any]any:
		for k, v := range mm {
			if match(k) {
				return v, true
			}
		}
	}
	return nil, false
}
