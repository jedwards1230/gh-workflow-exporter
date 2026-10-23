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
// `on` may be a string (`on: push`), a list (`on: [push, schedule]`, which
// cannot carry crons) or a map. YAML 1.1 parsers read a bare `on` key as the
// boolean true; yaml.v3 keeps it a string, but a key written as `true:` (or
// produced by a tool that round-tripped the file through YAML 1.1) is accepted
// as well.
func parseSchedules(content []byte) ([]string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, fmt.Errorf("invalid workflow YAML: %w", err)
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return nil, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("workflow is not a YAML mapping")
	}
	on := mapValue(root, isOnKey)
	if on == nil || on.Kind != yaml.MappingNode {
		return nil, nil
	}
	sched := mapValue(on, func(k *yaml.Node) bool { return k.Value == "schedule" })
	if sched == nil || sched.Tag == "!!null" {
		return nil, nil
	}
	if sched.Kind != yaml.SequenceNode {
		return nil, errors.New("on.schedule is not a list")
	}
	var crons []string
	for i, item := range sched.Content {
		if item.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("on.schedule[%d] is not a mapping", i)
		}
		c := mapValue(item, func(k *yaml.Node) bool { return k.Value == "cron" })
		if c == nil || c.Kind != yaml.ScalarNode || strings.TrimSpace(c.Value) == "" {
			return nil, fmt.Errorf("on.schedule[%d] has no cron", i)
		}
		crons = append(crons, c.Value)
	}
	return crons, nil
}

func isOnKey(k *yaml.Node) bool {
	if k.Kind != yaml.ScalarNode {
		return false
	}
	if k.Value == "on" {
		return true
	}
	return k.Tag == "!!bool" && strings.EqualFold(k.Value, "true")
}

// mapValue returns the value node of the first key in m matching match.
func mapValue(m *yaml.Node, match func(*yaml.Node) bool) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if match(m.Content[i]) {
			return m.Content[i+1]
		}
	}
	return nil
}
