// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config loads and validates the DirAdvisor config file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// OwnLabels are set by DirAdvisor and cannot be set from the config.
var OwnLabels = []string{"path", "source", "volume", "stack", "owner", "state", "project_id"}

type Config struct {
	Global      Global      `yaml:"global"`
	Docker      Docker      `yaml:"docker"`
	Directories []Directory `yaml:"directories"`
}

type Global struct {
	ReconcileInterval      Duration         `yaml:"reconcile_interval"`
	AdminActionLogInterval Duration         `yaml:"admin_action_log_interval"`
	WalkFilesPerSecond     int              `yaml:"walk_files_per_second"`
	ImageUpdateCheck       ImageUpdateCheck `yaml:"image_update_check"`
	ProjectIDs             IDRange          `yaml:"project_ids"`
}

type ImageUpdateCheck struct {
	Enabled  bool     `yaml:"enabled"`
	Interval Duration `yaml:"interval"`
}

type IDRange struct {
	From int64 `yaml:"from"`
	To   int64 `yaml:"to"`
}

type Docker struct {
	Enabled         bool        `yaml:"enabled"`
	Exclude         Exclude     `yaml:"exclude"`
	ContainerLabels []LabelRef  `yaml:"container_labels"`
	VolumeLabels    []LabelRef  `yaml:"volume_labels"`
	Labels          []LabelRule `yaml:"labels"`
}

type Exclude struct {
	Volumes []string `yaml:"volumes"`
}

// LabelRef is an allowlisted Docker label, written as "name" or {from, to}.
type LabelRef struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

func (r *LabelRef) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		r.From = n.Value
		return nil
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if v.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: %s must be a string", v.Line, k.Value)
			}
			switch k.Value {
			case "from":
				r.From = v.Value
			case "to":
				r.To = v.Value
			default:
				return fmt.Errorf("line %d: unknown key %q", k.Line, k.Value)
			}
		}
		return nil
	}
	return fmt.Errorf("line %d: expected a name or {from, to}", n.Line)
}

type LabelRule struct {
	Match  Match             `yaml:"match"`
	Labels map[string]string `yaml:"labels"`
}

type Match struct {
	Stack     string `yaml:"stack"`
	Volume    string `yaml:"volume"`
	Container string `yaml:"container"`
	Path      string `yaml:"path"`
}

type Directory struct {
	Path   string            `yaml:"path"`
	Labels map[string]string `yaml:"labels"`
}

// Duration is a time.Duration written as "60s", "1h", ...
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %v", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

func Default() *Config {
	return &Config{
		Global: Global{
			ReconcileInterval:      Duration(60 * time.Second),
			AdminActionLogInterval: Duration(time.Hour),
			ImageUpdateCheck:       ImageUpdateCheck{Interval: Duration(24 * time.Hour)},
			ProjectIDs:             IDRange{From: 100000, To: 199999},
		},
		Docker: Docker{Enabled: true},
	}
}

// StatFunc reports whether a host path exists and is a directory.
type StatFunc func(hostPath string) (exists, isDir bool)

// LookupFunc returns an environment variable.
type LookupFunc func(name string) (string, bool)

// Load reads the file. A missing file gives the defaults and found=false.
func Load(file string, lookup LookupFunc, stat StatFunc) (cfg *Config, found bool, err error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), false, nil
	}
	if err != nil {
		return nil, false, err
	}
	cfg, err = Parse(data, lookup, stat)
	return cfg, true, err
}

func Parse(data []byte, lookup LookupFunc, stat StatFunc) (*Config, error) {
	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if err := cfg.expand(lookup); err != nil {
		return nil, err
	}
	if err := cfg.validate(stat); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) expand(lookup LookupFunc) error {
	var err error
	ex := func(s *string) {
		if err != nil {
			return
		}
		*s, err = Expand(*s, lookup)
	}
	exMap := func(m map[string]string) {
		for k, v := range m {
			ex(&v)
			m[k] = v
		}
	}
	for i := range c.Docker.Exclude.Volumes {
		ex(&c.Docker.Exclude.Volumes[i])
	}
	for _, l := range [][]LabelRef{c.Docker.ContainerLabels, c.Docker.VolumeLabels} {
		for i := range l {
			ex(&l[i].From)
			ex(&l[i].To)
		}
	}
	for i := range c.Docker.Labels {
		r := &c.Docker.Labels[i]
		ex(&r.Match.Stack)
		ex(&r.Match.Volume)
		ex(&r.Match.Container)
		ex(&r.Match.Path)
		exMap(r.Labels)
	}
	for i := range c.Directories {
		ex(&c.Directories[i].Path)
		exMap(c.Directories[i].Labels)
	}
	return err
}

var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func validLabelName(n string) bool {
	return labelName.MatchString(n) && !strings.HasPrefix(n, "__")
}

func isOwn(n string) bool {
	for _, o := range OwnLabels {
		if n == o {
			return true
		}
	}
	return false
}

func checkLabelName(where, n string, allowStackOwner bool) error {
	if !validLabelName(n) {
		return fmt.Errorf("%s: invalid label name %q", where, n)
	}
	if isOwn(n) && !(allowStackOwner && (n == "stack" || n == "owner")) {
		return fmt.Errorf("%s: label %q is set by DirAdvisor", where, n)
	}
	return nil
}

func (c *Config) validate(stat StatFunc) error {
	g := c.Global
	if g.ReconcileInterval <= 0 || g.AdminActionLogInterval <= 0 || g.ImageUpdateCheck.Interval <= 0 {
		return errors.New("global: durations must be greater than 0")
	}
	if g.WalkFilesPerSecond < 0 || g.WalkFilesPerSecond > 1_000_000_000 {
		return errors.New("global.walk_files_per_second must be between 0 and 1000000000")
	}
	if g.ProjectIDs.From < 1 || g.ProjectIDs.To <= g.ProjectIDs.From || g.ProjectIDs.To > 4294967294 {
		return errors.New("global.project_ids: need 1 <= from < to <= 4294967294")
	}
	for _, l := range []struct {
		name, prefix string
		refs         []LabelRef
	}{
		{"docker.container_labels", "container_label_", c.Docker.ContainerLabels},
		{"docker.volume_labels", "volume_label_", c.Docker.VolumeLabels},
	} {
		seen := map[string]bool{}
		for _, r := range l.refs {
			if r.From == "" {
				return fmt.Errorf("%s: empty label name", l.name)
			}
			n := r.Target(l.prefix)
			if err := checkLabelName(l.name, n, false); err != nil {
				return err
			}
			if seen[n] {
				return fmt.Errorf("%s: two labels map to %q", l.name, n)
			}
			seen[n] = true
		}
	}
	for i, r := range c.Docker.Labels {
		for k := range r.Labels {
			if err := checkLabelName(fmt.Sprintf("docker.labels[%d]", i), k, false); err != nil {
				return err
			}
		}
	}
	for i, d := range c.Directories {
		where := fmt.Sprintf("directories[%d]", i)
		for k := range d.Labels {
			if err := checkLabelName(where, k, true); err != nil {
				return err
			}
		}
		p := d.Path
		if !path.IsAbs(p) {
			return fmt.Errorf("%s: path %q is not absolute", where, p)
		}
		if strings.Contains(p, "**") {
			return fmt.Errorf("%s: ** is not supported", where)
		}
		p = path.Clean(p)
		c.Directories[i].Path = p
		glob := strings.Contains(p, "*")
		if (!glob && Excluded(p, "")) || (glob && excludedTree(StaticPrefix(p), "")) {
			return fmt.Errorf("%s: %s is excluded", where, p)
		}
		if !glob && stat != nil {
			if exists, isDir := stat(p); exists && !isDir {
				return fmt.Errorf("%s: %s is not a directory", where, p)
			}
		}
	}
	return nil
}

// Target is the metric label name of an allowlisted Docker label.
func (r LabelRef) Target(prefix string) string {
	if r.To != "" {
		return r.To
	}
	return prefix + SanitizeLabelName(r.From)
}

// SanitizeLabelName replaces every character outside [a-zA-Z0-9_] by "_".
func SanitizeLabelName(s string) string {
	b := []byte(s)
	for i, ch := range b {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_') {
			b[i] = '_'
		}
	}
	return string(b)
}

var excludedTrees = []string{"/var/lib/docker", "/var/lib/containerd", "/proc", "/sys", "/dev", "/run"}

// Excluded reports whether a host path hits the built-in exclusion list.
func Excluded(p, dockerRoot string) bool {
	return path.Clean(p) == "/" || excludedTree(p, dockerRoot)
}

func excludedTree(p, dockerRoot string) bool {
	p = path.Clean(p)
	trees := excludedTrees
	if dockerRoot != "" {
		trees = append([]string{path.Clean(dockerRoot)}, trees...)
	}
	for _, t := range trees {
		if p == t || strings.HasPrefix(p, t+"/") {
			return true
		}
	}
	return false
}
