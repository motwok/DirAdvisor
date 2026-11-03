// SPDX-License-Identifier: AGPL-3.0-or-later

// Package docker is a minimal read-only client for the Docker Engine API.
package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MinAPIVersion is the oldest supported Docker API version.
const MinAPIVersion = "1.41"

type Info struct {
	DockerRootDir   string `json:"DockerRootDir"`
	OperatingSystem string `json:"OperatingSystem"`
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
	ID         string            `json:"Id"`
	Names      []string          `json:"Names"`
	ImageID    string            `json:"ImageID"`
	Labels     map[string]string `json:"Labels"`
	State      string            `json:"State"`
	Created    int64             `json:"Created"`
	Mounts     []Mount           `json:"Mounts"`
	HostConfig struct {
		NetworkMode string `json:"NetworkMode"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]Endpoint `json:"Networks"`
	} `json:"NetworkSettings"`
}

// Endpoint is a container's attachment to one Docker network.
type Endpoint struct {
	EndpointID          string `json:"EndpointID"`
	IPAddress           string `json:"IPAddress"`
	IPPrefixLen         int    `json:"IPPrefixLen"`
	GlobalIPv6Address   string `json:"GlobalIPv6Address"`
	GlobalIPv6PrefixLen int    `json:"GlobalIPv6PrefixLen"`
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
	State struct {
		Pid int `json:"Pid"`
	} `json:"State"`
	HostConfig struct {
		NanoCpus int64 `json:"NanoCpus"`
		Memory   int64 `json:"Memory"`
	} `json:"HostConfig"`
}

// Stats is the part of a one-shot stats answer HatchLens reads (Windows).
type Stats struct {
	CPUStats struct {
		CPUUsage struct {
			// 100 ns units on Windows.
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
	} `json:"cpu_stats"`
	MemoryStats struct {
		Commit            uint64 `json:"commitbytes"`
		PrivateWorkingSet uint64 `json:"privateworkingset"`
	} `json:"memory_stats"`
	StorageStats struct {
		ReadBytes  uint64 `json:"read_size_bytes"`
		WriteBytes uint64 `json:"write_size_bytes"`
		Reads      uint64 `json:"read_count_normalized"`
		Writes     uint64 `json:"write_count_normalized"`
	} `json:"storage_stats"`
	Networks map[string]NetStats `json:"networks"`
}

type NetStats struct {
	RxBytes   uint64 `json:"rx_bytes"`
	RxPackets uint64 `json:"rx_packets"`
	RxErrors  uint64 `json:"rx_errors"`
	RxDropped uint64 `json:"rx_dropped"`
	TxBytes   uint64 `json:"tx_bytes"`
	TxPackets uint64 `json:"tx_packets"`
	TxErrors  uint64 `json:"tx_errors"`
	TxDropped uint64 `json:"tx_dropped"`
}

type Image struct {
	ID          string   `json:"Id"`
	RepoDigests []string `json:"RepoDigests"`
}

type Event struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
}

// Relevant reports whether an event can change what HatchLens reports.
func (e Event) Relevant() bool {
	switch e.Type {
	case "volume":
		return e.Action == "create" || e.Action == "destroy"
	case "container":
		switch e.Action {
		case "create", "start", "stop", "die", "destroy":
			return true
		}
	case "image":
		switch e.Action {
		case "pull", "tag", "untag", "delete":
			return true
		}
	case "network":
		return e.Action == "connect" || e.Action == "disconnect"
	}
	return false
}

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

type Client struct {
	hc     *http.Client
	stream *http.Client
	mu     sync.Mutex
	// version is the API version in the request path: MinAPIVersion, or the
	// daemon's minimum if that is higher (CheckVersion).
	version string
}

func (c *Client) apiVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version
}

// New creates a client for the Docker socket (Linux) or named pipe (Windows).
func New(socket string) *Client {
	d := dialer(socket)
	return &Client{
		hc:      &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DialContext: d}},
		stream:  &http.Client{Transport: &http.Transport{DialContext: d}},
		version: MinAPIVersion,
	}
}

// Dial connects to a Unix socket (Linux) or named pipe (Windows); also for the
// host service.
func Dial(socket string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return dialer(socket)
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

func (c *Client) do(ctx context.Context, p string, header http.Header, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/v"+c.apiVersion()+p, nil)
	if err != nil {
		return err
	}
	for k, v := range header {
		req.Header[k] = v
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

// IsNotFound reports whether err is a 404 from the Docker API.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// CheckVersion reads the daemon's API version and fails below MinAPIVersion.
// reached is true only if the daemon answered with a version, so any other
// failure is retried instead of read as "too old". A daemon whose minimum is
// above MinAPIVersion (Docker 29.0 to 29.2: 1.44) refuses lower path versions,
// so its minimum is used from then on.
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
		return "", false, apiError(resp)
	}
	var v struct {
		APIVersion    string `json:"ApiVersion"`
		MinAPIVersion string `json:"MinAPIVersion"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil || v.APIVersion == "" {
		return "", false, fmt.Errorf("no API version in the /version answer: %v", err)
	}
	if CompareVersion(v.APIVersion, MinAPIVersion) < 0 {
		return v.APIVersion, true, fmt.Errorf("Docker API %s is older than the required %s", v.APIVersion, MinAPIVersion)
	}
	if CompareVersion(v.MinAPIVersion, MinAPIVersion) > 0 {
		c.mu.Lock()
		c.version = v.MinAPIVersion
		c.mu.Unlock()
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
	err := c.do(ctx, "/info", nil, &i)
	return i, err
}

func (c *Client) Volumes(ctx context.Context) ([]Volume, error) {
	var r struct {
		Volumes []Volume `json:"Volumes"`
	}
	err := c.do(ctx, "/volumes", nil, &r)
	return r.Volumes, err
}

// Network is a Docker network; on Windows Options holds the HNS network ID.
type Network struct {
	Name    string            `json:"Name"`
	ID      string            `json:"Id"`
	Options map[string]string `json:"Options"`
}

func (c *Client) Networks(ctx context.Context) ([]Network, error) {
	var r []Network
	err := c.do(ctx, "/networks", nil, &r)
	return r, err
}

func (c *Client) Containers(ctx context.Context) ([]Container, error) {
	var r []Container
	err := c.do(ctx, "/containers/json?all=1", nil, &r)
	return r, err
}

func (c *Client) Container(ctx context.Context, id string) (ContainerJSON, error) {
	var r ContainerJSON
	err := c.do(ctx, "/containers/"+url.PathEscape(id)+"/json", nil, &r)
	return r, err
}

// Stats reads one sample of a running container's resource counters.
func (c *Client) Stats(ctx context.Context, id string) (Stats, error) {
	var r Stats
	err := c.do(ctx, "/containers/"+url.PathEscape(id)+"/stats?stream=false&one-shot=true", nil, &r)
	return r, err
}

// Image inspects an image by ID or reference; an unknown one is an *APIError with Status 404.
func (c *Client) Image(ctx context.Context, idOrRef string) (Image, error) {
	var r Image
	err := c.do(ctx, "/images/"+idOrRef+"/json", nil, &r)
	return r, err
}

// Distribution asks the daemon for the registry digest of an image reference.
// auth is the X-Registry-Auth value, or empty.
func (c *Client) Distribution(ctx context.Context, ref, auth string) (string, error) {
	var r struct {
		Descriptor struct {
			Digest string `json:"digest"`
		} `json:"Descriptor"`
	}
	var h http.Header
	if auth != "" {
		h = http.Header{"X-Registry-Auth": {auth}}
	}
	err := c.do(ctx, "/distribution/"+ref+"/json", h, &r)
	return r.Descriptor.Digest, err
}

// Events streams container, image, volume and network events until ctx ends or the stream breaks.
func (c *Client) Events(ctx context.Context, fn func(Event)) error {
	filters := url.QueryEscape(`{"type":["container","image","volume","network"]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/v"+c.apiVersion()+"/events?filters="+filters, nil)
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
