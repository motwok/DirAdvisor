// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config loads and validates the HatchLens config.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/motwok/HatchLens/internal/docker"
	"github.com/motwok/HatchLens/internal/ref"
	"go.yaml.in/yaml/v3"
)

// OwnLabels are set by HatchLens on the directory metrics and cannot be set from the config.
var OwnLabels = []string{"path", "source", "volume", "stack", "owner", "state", "project_id"}

// ImageOwnLabels are set by HatchLens on the image metrics.
var ImageOwnLabels = []string{"container", "image", "reference", "stack", "state", "mode"}

// ContainerOwnLabels are set by HatchLens on the CPU, memory, disk and network metrics.
var ContainerOwnLabels = []string{"container", "stack", "network", "interface"}

// Functions that can be switched off with disable.
const (
	FnDirectory = "directory"
	FnVolume    = "volume"
	FnImage     = "image"
	FnCPU       = "cpu"
	FnRAM       = "ram"
	FnDisk      = "disk"
	FnNet       = "net"
)

var functions = []string{FnDirectory, FnVolume, FnImage, FnCPU, FnRAM, FnDisk, FnNet}

// ContainerFunctions are the per-container resource functions.
var ContainerFunctions = []string{FnCPU, FnRAM, FnDisk, FnNet}

// Docker labels HatchLens reads on containers and volumes.
const (
	LabelEnable = "de.mo2000.hatchlens.enable"
	LabelSelf   = "de.mo2000.hatchlens.label."
)

type Config struct {
	Disable         []string `yaml:"disable"`
	Global          `yaml:",inline"`
	Exclude         Exclude     `yaml:"exclude"`
	ContainerLabels []LabelRef  `yaml:"container_labels"`
	VolumeLabels    []LabelRef  `yaml:"volume_labels"`
	Labels          []LabelRule `yaml:"labels"`
	DockerSocket    string      `yaml:"docker_socket"`
	HostSocket      string      `yaml:"host_socket"`
	Directories     []Directory `yaml:"directories"`
	Images          Images      `yaml:"images"`
}

type Images struct {
	CheckInterval Duration   `yaml:"check_interval"`
	ForgetAfter   Duration   `yaml:"forget_after"`
	Registries    []Registry `yaml:"registries"`
}

type Registry struct {
	Host     string `yaml:"host"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Registry returns the credentials for a normalized registry host, if any.
func (im Images) Registry(host string) (Registry, bool) {
	for _, r := range im.Registries {
		if ref.NormalizeHost(r.Host) == host {
			return r, true
		}
	}
	return Registry{}, false
}

// MatchAny reports whether any of the name globs matches any of the names.
func MatchAny(patterns []string, names ...string) bool {
	for _, p := range patterns {
		for _, n := range names {
			if MatchName(p, n) {
				return true
			}
		}
	}
	return false
}

// IsImageOwn reports whether n is an own label of the image metrics.
func IsImageOwn(n string) bool { return slices.Contains(ImageOwnLabels, n) }

// IsContainerOwn reports whether n is an own label of the container resource metrics.
func IsContainerOwn(n string) bool { return slices.Contains(ContainerOwnLabels, n) }

type Global struct {
	ListenAddress          string   `yaml:"listen_address"`
	LogLevel               string   `yaml:"log_level"`
	LogFormat              string   `yaml:"log_format"`
	StoragePath            string   `yaml:"storage_path"`
	RootFS                 string   `yaml:"rootfs"`
	ReconcileInterval      Duration `yaml:"reconcile_interval"`
	AdminActionLogInterval Duration `yaml:"admin_action_log_interval"`
	WalkFilesPerSecond     int      `yaml:"walk_files_per_second"`
	ProjectIDs             IDRange  `yaml:"project_ids"`
}

type IDRange struct {
	From int64 `yaml:"from"`
	To   int64 `yaml:"to"`
}

type Exclude struct {
	Volumes    []string `yaml:"volumes"`
	Containers []string `yaml:"containers"`
	Images     []string `yaml:"images"`
}

// ExcludedContainer reports whether a container is excluded from every
// function: by name, by its image reference (as written or normalized) or by
// the enable label.
func (c *Config) ExcludedContainer(name string, labels map[string]string, images ...string) bool {
	return labels[LabelEnable] == "false" || MatchAny(c.Exclude.Containers, name) || MatchAny(c.Exclude.Images, images...)
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
	Image     string `yaml:"image"`
	Path      string `yaml:"path"`
}

// Subject is what a rule is matched against. A nil field or no Images means
// the series has no such key, so a rule naming it does not match.
type Subject struct {
	Stack, Container, Volume, Path *string
	Images                         []string
}

// Matches reports whether every key given in the rule exists for s and matches.
func (m Match) Matches(s Subject) bool {
	key := func(p string, v *string, match func(p, v string) bool) bool {
		return p == "" || v != nil && match(p, *v)
	}
	return key(m.Stack, s.Stack, MatchName) && key(m.Container, s.Container, MatchName) &&
		key(m.Volume, s.Volume, MatchName) && key(m.Path, s.Path, MatchPath) &&
		(m.Image == "" || MatchAny([]string{m.Image}, s.Images...))
}

// ExtraLabels resolves the additional labels of a container series: allowlist,
// then self-service, then rules; later ones win. Names that are invalid or own
// labels (own) are skipped and reported to skip.
func (c *Config) ExtraLabels(name string, labels map[string]string, stack string, images []string,
	own func(string) bool, skip func(label string)) map[string]string {
	out := map[string]string{}
	set := func(n, v string) {
		if !ValidLabelName(n) || own(n) {
			skip(n)
			return
		}
		out[n] = v
	}
	for _, l := range c.ContainerLabels {
		if v, ok := labels[l.From]; ok {
			set(l.Target("container_label_"), v)
		}
	}
	for k, v := range labels {
		if n, ok := strings.CutPrefix(k, LabelSelf); ok {
			set(n, v)
		}
	}
	for k, v := range c.RuleLabels(Subject{Stack: &stack, Container: &name, Images: images}) {
		out[k] = v
	}
	return out
}

// RuleLabels returns the labels of all rules matching s, later rules winning.
func (c *Config) RuleLabels(s Subject) map[string]string {
	out := map[string]string{}
	for _, r := range c.Labels {
		if r.Match.Matches(s) {
			for k, v := range r.Labels {
				out[k] = v
			}
		}
	}
	return out
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
			ListenAddress:          ":8080",
			LogLevel:               "info",
			LogFormat:              "logfmt",
			StoragePath:            defaultStorage,
			RootFS:                 "/host",
			ReconcileInterval:      Duration(60 * time.Second),
			AdminActionLogInterval: Duration(time.Hour),
			ProjectIDs:             IDRange{From: 100000, To: 199999},
		},
		DockerSocket: docker.DefaultSocket,
		HostSocket:   defaultHostSocket,
		Images:       Images{CheckInterval: Duration(24 * time.Hour), ForgetAfter: Duration(168 * time.Hour)},
	}
}

// StatFunc reports whether a host path exists and is a directory.
type StatFunc func(hostPath string) (exists, isDir bool)

// LookupFunc returns an environment variable.
type LookupFunc func(name string) (string, bool)

// Source resolves CFG-2: -config, then HATCHLENS_CONFIG_FILE, then HATCHLENS_CONFIG. Without any,
// data is empty (the defaults) and src is "defaults".
func Source(flagPath string, getenv func(string) string) (data []byte, src string, err error) {
	if flagPath != "" {
		data, err = os.ReadFile(flagPath)
		return data, flagPath, err
	}
	if p := getenv("HATCHLENS_CONFIG_FILE"); p != "" {
		data, err = os.ReadFile(p)
		return data, p, err
	}
	if c := getenv("HATCHLENS_CONFIG"); c != "" {
		return []byte(c), "HATCHLENS_CONFIG", nil
	}
	return nil, "defaults", nil
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

// expand replaces the variables in all string values; an error names the key, never the value.
func (c *Config) expand(lookup LookupFunc) error {
	var err error
	ex := func(where string, s *string) {
		if err == nil {
			if *s, err = Expand(*s, lookup); err != nil {
				err = fmt.Errorf("%s: %w", where, err)
			}
		}
	}
	exMap := func(where string, m map[string]string) {
		for k, v := range m {
			ex(where+".labels."+k, &v)
			m[k] = v
		}
	}
	g := &c.Global
	ex("listen_address", &g.ListenAddress)
	ex("log_level", &g.LogLevel)
	ex("log_format", &g.LogFormat)
	ex("storage_path", &g.StoragePath)
	ex("rootfs", &g.RootFS)
	ex("docker_socket", &c.DockerSocket)
	ex("host_socket", &c.HostSocket)
	for _, l := range []struct {
		name string
		list []string
	}{{"exclude.volumes", c.Exclude.Volumes}, {"exclude.containers", c.Exclude.Containers}, {"exclude.images", c.Exclude.Images}} {
		for i := range l.list {
			ex(fmt.Sprintf("%s[%d]", l.name, i), &l.list[i])
		}
	}
	for _, l := range []struct {
		name string
		refs []LabelRef
	}{{"container_labels", c.ContainerLabels}, {"volume_labels", c.VolumeLabels}} {
		for i := range l.refs {
			where := fmt.Sprintf("%s[%d]", l.name, i)
			ex(where+".from", &l.refs[i].From)
			ex(where+".to", &l.refs[i].To)
		}
	}
	for i := range c.Labels {
		r, where := &c.Labels[i], fmt.Sprintf("labels[%d]", i)
		ex(where+".match.stack", &r.Match.Stack)
		ex(where+".match.volume", &r.Match.Volume)
		ex(where+".match.container", &r.Match.Container)
		ex(where+".match.image", &r.Match.Image)
		ex(where+".match.path", &r.Match.Path)
		exMap(where, r.Labels)
	}
	for i := range c.Directories {
		where := fmt.Sprintf("directories[%d]", i)
		ex(where+".path", &c.Directories[i].Path)
		exMap(where, c.Directories[i].Labels)
	}
	im := &c.Images
	for i := range im.Registries {
		r, where := &im.Registries[i], fmt.Sprintf("images.registries[%d]", i)
		ex(where+".host", &r.Host)
		ex(where+".username", &r.Username)
		ex(where+".password", &r.Password)
	}
	return err
}

var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// ValidLabelName reports whether n is a Prometheus label name that is not reserved.
func ValidLabelName(n string) bool {
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

// isAnyOwn: own label of the directory or the image metrics; shared rules and
// the container allowlist feed both.
func isAnyOwn(n string) bool { return isOwn(n) || IsImageOwn(n) || IsContainerOwn(n) }

func checkName(where, n string, own func(string) bool) error {
	if !ValidLabelName(n) {
		return fmt.Errorf("%s: invalid label name %q", where, n)
	}
	if own(n) {
		return fmt.Errorf("%s: label %q is set by HatchLens", where, n)
	}
	return nil
}

func checkLabelName(where, n string, allowStackOwner bool) error {
	if !ValidLabelName(n) {
		return fmt.Errorf("%s: invalid label name %q", where, n)
	}
	if isOwn(n) && !(allowStackOwner && (n == "stack" || n == "owner")) {
		return fmt.Errorf("%s: label %q is set by HatchLens", where, n)
	}
	return nil
}

// Enabled reports whether a function is not in disable.
func (c *Config) Enabled(f string) bool {
	return !slices.Contains(c.Disable, f)
}

// Runs reports whether a function runs; every enabled function runs, on
// Windows the directory and volume parts through the host service.
func (c *Config) Runs(f string) bool {
	return c.Enabled(f)
}

// RunsAny reports whether one of fs runs.
func (c *Config) RunsAny(fs ...string) bool {
	return slices.ContainsFunc(fs, c.Runs)
}

func (c *Config) validate(stat StatFunc) error {
	for _, f := range c.Disable {
		if !slices.Contains(functions, f) {
			return fmt.Errorf("disable: unknown function %q, want one of %s", f, strings.Join(functions, ", "))
		}
	}
	if c.DockerSocket == "" {
		var need []string
		for _, f := range []string{FnVolume, FnImage, FnCPU, FnRAM, FnDisk, FnNet} {
			if c.Runs(f) {
				need = append(need, f)
			}
		}
		if len(need) > 0 {
			return fmt.Errorf("docker_socket is empty, but %s need it; add them to disable", strings.Join(need, ", "))
		}
	}
	if windows && c.HostSocket == "" {
		var need []string
		for _, f := range []string{FnDirectory, FnVolume} {
			if c.Runs(f) {
				need = append(need, f)
			}
		}
		if len(need) > 0 {
			return fmt.Errorf("host_socket is empty, but %s need it; add them to disable", strings.Join(need, ", "))
		}
	}
	g := c.Global
	switch g.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level %q: want debug, info, warn or error", g.LogLevel)
	}
	if g.LogFormat != "logfmt" && g.LogFormat != "json" {
		return fmt.Errorf("log_format %q: want logfmt or json", g.LogFormat)
	}
	if g.ReconcileInterval <= 0 || g.AdminActionLogInterval <= 0 {
		return errors.New("durations must be greater than 0")
	}
	if err := c.Images.validate(); err != nil {
		return err
	}
	if g.WalkFilesPerSecond < 0 || g.WalkFilesPerSecond > 1_000_000_000 {
		return errors.New("walk_files_per_second must be between 0 and 1000000000")
	}
	if g.ProjectIDs.From < 1 || g.ProjectIDs.To <= g.ProjectIDs.From || g.ProjectIDs.To > 4294967294 {
		return errors.New("project_ids: need 1 <= from < to <= 4294967294")
	}
	for _, l := range []struct {
		name, prefix string
		refs         []LabelRef
		own          func(string) bool
	}{
		{"container_labels", "container_label_", c.ContainerLabels, isAnyOwn},
		{"volume_labels", "volume_label_", c.VolumeLabels, isOwn},
	} {
		seen := map[string]bool{}
		for i, r := range l.refs {
			where := fmt.Sprintf("%s[%d]", l.name, i)
			if r.From == "" {
				return fmt.Errorf("%s: empty label name", where)
			}
			n := r.Target(l.prefix)
			if err := checkName(where, n, l.own); err != nil {
				return err
			}
			if seen[n] {
				return fmt.Errorf("%s: two labels map to %q", l.name, n)
			}
			seen[n] = true
		}
	}
	for i, r := range c.Labels {
		for k := range r.Labels {
			if err := checkName(fmt.Sprintf("labels[%d]", i), k, isAnyOwn); err != nil {
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
		p := Host(d.Path)
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
	}
	if stat != nil {
		return c.CheckDirectories(stat)
	}
	return nil
}

func (im Images) validate() error {
	if im.CheckInterval <= 0 || im.ForgetAfter <= 0 {
		return errors.New("images: durations must be greater than 0")
	}
	hosts := map[string]bool{}
	for i, r := range im.Registries {
		h := ref.NormalizeHost(r.Host)
		if h == "" {
			return fmt.Errorf("images.registries[%d]: host missing", i)
		}
		if hosts[h] {
			return fmt.Errorf("images.registries[%d]: host %s twice", i, h)
		}
		hosts[h] = true
	}
	return nil
}

// CheckDirectories rejects configured directories that exist and are not directories.
func (c *Config) CheckDirectories(stat StatFunc) error {
	for i, d := range c.Directories {
		if strings.Contains(d.Path, "*") {
			continue
		}
		if exists, isDir := stat(d.Path); exists && !isDir {
			return fmt.Errorf("directories[%d]: %s is not a directory", i, d.Path)
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
	return path.Clean(p) == "/" || driveRoot(path.Clean(p)) || excludedTree(p, dockerRoot)
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
