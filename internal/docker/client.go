// SPDX-License-Identifier: AGPL-3.0-or-later

// Package docker is a minimal read-only client for the Docker Engine API.
package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MinAPIVersion is the oldest supported Docker API version.
const MinAPIVersion = "1.41"

type Info struct {
	DockerRootDir string `json:"DockerRootDir"`
}

type Volume struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Mountpoint string            `json:"Mountpoint"`
	CreatedAt  string            `json:"CreatedAt"`
	Labels     map[string]string `json:"Labels"`
	Options    map[string]string `json:"Options"`
}

type Mount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

type Container struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Labels  map[string]string `json:"Labels"`
	State   string            `json:"State"`
	Created int64             `json:"Created"`
	Mounts  []Mount           `json:"Mounts"`
}

// Name is the container name without the leading slash. Legacy links add
// aliases like "/other/alias" in no fixed order; those are skipped.
func (c Container) Name() string {
	if len(c.Names) == 0 {
		return c.ID
	}
	for _, n := range c.Names {
		if n = strings.TrimPrefix(n, "/"); !strings.Contains(n, "/") {
			return n
		}
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

type ContainerJSON struct {
	ID     string `json:"Id"`
	Image  string `json:"Image"`
	Config struct {
		Image string `json:"Image"`
	} `json:"Config"`
}

type Client struct {
	hc      *http.Client
	stream  *http.Client
	version string
}

// New creates a client for a Unix socket.
func New(socket string) *Client {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}
	return &Client{
		hc:      &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DialContext: dial}},
		stream:  &http.Client{Transport: &http.Transport{DialContext: dial}},
		version: MinAPIVersion,
	}
}

// NewWithHTTP uses a custom transport (tests).
func NewWithHTTP(rt http.RoundTripper) *Client {
	return &Client{hc: &http.Client{Transport: rt}, stream: &http.Client{Transport: rt}, version: MinAPIVersion}
}

type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("docker API %d: %s", e.Status, e.Message) }

func (c *Client) get(ctx context.Context, p string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/v"+c.version+p, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func apiError(resp *http.Response) error {
	var m struct {
		Message string `json:"message"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if json.Unmarshal(b, &m) != nil || m.Message == "" {
		m.Message = strings.TrimSpace(string(b))
	}
	return &APIError{Status: resp.StatusCode, Message: m.Message}
}

// CheckVersion reads the daemon's API version and fails below MinAPIVersion.
// It returns the version and whether the daemon was reached.
func (c *Client) CheckVersion(ctx context.Context) (version string, reached bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/version", nil)
	if err != nil {
		return "", false, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// A socket proxy denying /version must not read as "too old".
		return "", true, apiError(resp)
	}
	var v struct {
		APIVersion string `json:"ApiVersion"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", true, err
	}
	if CompareVersion(v.APIVersion, MinAPIVersion) < 0 {
		return v.APIVersion, true, fmt.Errorf("Docker API %s is older than the required %s", v.APIVersion, MinAPIVersion)
	}
	return v.APIVersion, true, nil
}

// CompareVersion compares dotted versions numerically.
func CompareVersion(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func (c *Client) Info(ctx context.Context) (Info, error) {
	var i Info
	err := c.get(ctx, "/info", &i)
	return i, err
}

func (c *Client) Volumes(ctx context.Context) ([]Volume, error) {
	var r struct {
		Volumes []Volume `json:"Volumes"`
	}
	err := c.get(ctx, "/volumes", &r)
	return r.Volumes, err
}

func (c *Client) Containers(ctx context.Context) ([]Container, error) {
	var r []Container
	err := c.get(ctx, "/containers/json?all=1", &r)
	return r, err
}

func (c *Client) Container(ctx context.Context, id string) (ContainerJSON, error) {
	var r ContainerJSON
	err := c.get(ctx, "/containers/"+url.PathEscape(id)+"/json", &r)
	return r, err
}

func (c *Client) ImageRepoDigests(ctx context.Context, id string) ([]string, error) {
	var r struct {
		RepoDigests []string `json:"RepoDigests"`
	}
	err := c.get(ctx, "/images/"+url.PathEscape(id)+"/json", &r)
	return r.RepoDigests, err
}

// DistributionDigest asks the daemon for the registry digest of an image reference.
func (c *Client) DistributionDigest(ctx context.Context, ref string) (string, error) {
	var r struct {
		Descriptor struct {
			Digest string `json:"digest"`
		} `json:"Descriptor"`
	}
	err := c.get(ctx, "/distribution/"+ref+"/json", &r)
	return r.Descriptor.Digest, err
}

type Event struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
}

// Relevant reports whether an event changes volumes or ownership.
func (e Event) Relevant() bool {
	switch e.Type {
	case "volume":
		return e.Action == "create" || e.Action == "destroy"
	case "container":
		switch e.Action {
		case "create", "destroy", "start", "stop", "die":
			return true
		}
	}
	return false
}

// Events streams events until ctx ends or the stream breaks.
func (c *Client) Events(ctx context.Context, fn func(Event)) error {
	filters := url.QueryEscape(`{"type":["container","volume"]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/v"+c.version+"/events?filters="+filters, nil)
	if err != nil {
		return err
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &APIError{Status: resp.StatusCode, Message: "events"}
	}
	dec := json.NewDecoder(bufio.NewReader(resp.Body))
	for {
		var e Event
		if err := dec.Decode(&e); err != nil {
			return err
		}
		fn(e)
	}
}
