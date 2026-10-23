// Package config loads the exporter's YAML config file and the GitHub App
// credentials.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Defaults applied when the config file omits a value.
const (
	DefaultPollInterval = 5 * time.Minute
	DefaultGraceFactor  = 1.5
)

// Duration is a time.Duration that unmarshals from a Go duration string
// such as "5m" or "90s".
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return fmt.Errorf("duration: %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// WorkflowRef names one workflow: a repository full name and the workflow
// file basename (e.g. "nightly.yml").
type WorkflowRef struct {
	Repo     string `yaml:"repo"`
	Workflow string `yaml:"workflow"`
}

// Key returns the case-insensitive identity used for matching.
func (r WorkflowRef) Key() string {
	return Key(r.Repo, r.Workflow)
}

// Key builds the case-insensitive matching key for a repo and workflow file
// basename. GitHub repository names are case-insensitive.
func Key(repo, workflow string) string {
	return strings.ToLower(repo) + "\x00" + strings.ToLower(workflow)
}

// Override adjusts how one workflow is tracked.
type Override struct {
	WorkflowRef `yaml:",inline"`
	// Branch is the branch whose runs are tracked. Empty means the repo's
	// default branch.
	Branch string `yaml:"branch"`
	// GraceFactor replaces default_grace_factor when set.
	GraceFactor *float64 `yaml:"grace_factor"`
}

// Config is the parsed config file.
type Config struct {
	PollInterval       Duration      `yaml:"poll_interval"`
	DefaultGraceFactor float64       `yaml:"default_grace_factor"`
	Include            []WorkflowRef `yaml:"include"`
	Exclude            []WorkflowRef `yaml:"exclude"`
	Overrides          []Override    `yaml:"overrides"`

	include   map[string]bool
	exclude   map[string]bool
	overrides map[string]Override
}

// Default returns a Config with every default applied and no lists.
func Default() *Config {
	c := &Config{PollInterval: Duration(DefaultPollInterval), DefaultGraceFactor: DefaultGraceFactor}
	if err := c.finish(); err != nil {
		panic(err) // unreachable: defaults are valid
	}
	return c
}

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied config path
	if err != nil {
		return nil, fmt.Errorf("opening config: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file
	cfg, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes and validates a config document. Unknown keys are rejected
// so a typo cannot silently disable a setting.
func Parse(r io.Reader) (*Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	cfg := &Config{}
	if len(bytes.TrimSpace(data)) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("decoding config: %w", err)
		}
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = Duration(DefaultPollInterval)
	}
	if cfg.DefaultGraceFactor == 0 {
		cfg.DefaultGraceFactor = DefaultGraceFactor
	}
	if err := cfg.finish(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// finish validates the config and builds the lookup maps.
func (c *Config) finish() error {
	var errs []error
	if c.PollInterval <= 0 {
		errs = append(errs, fmt.Errorf("poll_interval must be positive, got %s", time.Duration(c.PollInterval)))
	}
	if c.DefaultGraceFactor <= 0 {
		errs = append(errs, fmt.Errorf("default_grace_factor must be positive, got %v", c.DefaultGraceFactor))
	}
	c.include = map[string]bool{}
	c.exclude = map[string]bool{}
	c.overrides = map[string]Override{}
	for i, r := range c.Include {
		if err := r.validate(); err != nil {
			errs = append(errs, fmt.Errorf("include[%d]: %w", i, err))
		}
		c.include[r.Key()] = true
	}
	for i, r := range c.Exclude {
		if err := r.validate(); err != nil {
			errs = append(errs, fmt.Errorf("exclude[%d]: %w", i, err))
		}
		c.exclude[r.Key()] = true
	}
	for i, o := range c.Overrides {
		if err := o.validate(); err != nil {
			errs = append(errs, fmt.Errorf("overrides[%d]: %w", i, err))
		}
		if o.GraceFactor != nil && *o.GraceFactor <= 0 {
			errs = append(errs, fmt.Errorf("overrides[%d]: grace_factor must be positive, got %v", i, *o.GraceFactor))
		}
		if _, dup := c.overrides[o.Key()]; dup {
			errs = append(errs, fmt.Errorf("overrides[%d]: duplicate override for %s %s", i, o.Repo, o.Workflow))
		}
		c.overrides[o.Key()] = o
	}
	return errors.Join(errs...)
}

func (r WorkflowRef) validate() error {
	owner, name, ok := strings.Cut(r.Repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") || strings.ContainsAny(r.Repo, " \t") {
		return fmt.Errorf("repo %q must be in owner/name form", r.Repo)
	}
	if r.Workflow == "" {
		return errors.New("workflow must be set")
	}
	if strings.Contains(r.Workflow, "/") {
		return fmt.Errorf("workflow %q must be a file basename, not a path", r.Workflow)
	}
	return nil
}

// Interval returns the poll interval.
func (c *Config) Interval() time.Duration { return time.Duration(c.PollInterval) }

// Included reports whether repo/workflow is on the include list.
func (c *Config) Included(repo, workflow string) bool { return c.include[Key(repo, workflow)] }

// Excluded reports whether repo/workflow is on the exclude list.
func (c *Config) Excluded(repo, workflow string) bool { return c.exclude[Key(repo, workflow)] }

// Branch returns the override branch for repo/workflow, or defaultBranch.
func (c *Config) Branch(repo, workflow, defaultBranch string) string {
	if o, ok := c.overrides[Key(repo, workflow)]; ok && o.Branch != "" {
		return o.Branch
	}
	return defaultBranch
}

// GraceFactor returns the effective grace factor for repo/workflow.
func (c *Config) GraceFactor(repo, workflow string) float64 {
	if o, ok := c.overrides[Key(repo, workflow)]; ok && o.GraceFactor != nil {
		return *o.GraceFactor
	}
	return c.DefaultGraceFactor
}
