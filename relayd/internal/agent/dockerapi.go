// Package agent implements the relayd worker: inventory detection, the
// outbound session to the server, and job execution in Docker containers.
package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// dockerClient is a deliberately minimal Docker Engine API client over the
// unix socket. We need six endpoints; importing the full Docker SDK would
// drag in a hundred modules for them. Engine API version is pinned low for
// broad daemon compatibility.
const dockerAPIVersion = "v1.41"

type dockerClient struct {
	http *http.Client
	host string
}

func newDockerClient(socketPath string) *dockerClient {
	if socketPath == "" {
		socketPath = "/var/run/docker.sock"
	}
	return &dockerClient{
		host: "http://docker",
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

func (c *dockerClient) url(path string, query url.Values) string {
	u := c.host + "/" + dockerAPIVersion + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

func (c *dockerClient) do(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path, query), reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

func (c *dockerClient) errorFrom(resp *http.Response) error {
	defer resp.Body.Close()
	var payload struct {
		Message string `json:"message"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if json.Unmarshal(data, &payload) == nil && payload.Message != "" {
		return fmt.Errorf("docker: %s (%d)", payload.Message, resp.StatusCode)
	}
	return fmt.Errorf("docker: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
}

func (c *dockerClient) Ping(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/_ping", nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return c.errorFrom(resp)
	}
	return nil
}

func (c *dockerClient) ImageExists(ctx context.Context, ref string) (bool, error) {
	resp, err := c.do(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("docker image inspect: HTTP %d", resp.StatusCode)
	}
}

// ListImageTags returns local tags matching the given repository prefix.
func (c *dockerClient) ListImageTags(ctx context.Context, repoPrefix string) ([]string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/images/json", nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.errorFrom(resp)
	}
	var images []struct {
		RepoTags []string `json:"RepoTags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&images); err != nil {
		return nil, err
	}
	var tags []string
	for _, img := range images {
		for _, tag := range img.RepoTags {
			if strings.HasPrefix(tag, repoPrefix) {
				tags = append(tags, tag)
			}
		}
	}
	return tags, nil
}

// LocalImage is one locally stored image built or pulled by Relay.
type LocalImage struct {
	Tag       string
	ID        string
	CreatedAt time.Time
	SizeBytes int64
}

// ListImages returns local images carrying a tag with the given repository
// prefix, one entry per matching tag.
func (c *dockerClient) ListImages(ctx context.Context, repoPrefix string) ([]LocalImage, error) {
	resp, err := c.do(ctx, http.MethodGet, "/images/json", nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.errorFrom(resp)
	}
	var images []struct {
		Id       string   `json:"Id"`
		RepoTags []string `json:"RepoTags"`
		Created  int64    `json:"Created"`
		Size     int64    `json:"Size"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&images); err != nil {
		return nil, err
	}
	var out []LocalImage
	for _, img := range images {
		for _, tag := range img.RepoTags {
			if strings.HasPrefix(tag, repoPrefix) {
				out = append(out, LocalImage{
					Tag:       tag,
					ID:        img.Id,
					CreatedAt: time.Unix(img.Created, 0),
					SizeBytes: img.Size,
				})
			}
		}
	}
	return out, nil
}

// ImageRemove deletes one image by reference. A 409 means a container still
// uses it, which is not an error worth surfacing: the sweep simply skips it
// and tries again next time.
func (c *dockerClient) ImageRemove(ctx context.Context, ref string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/images/"+url.PathEscape(ref), nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNotFound:
		return nil
	default:
		return c.errorFrom(resp)
	}
}

type deviceRequest struct {
	Driver       string     `json:"Driver,omitempty"`
	Count        int        `json:"Count,omitempty"`
	DeviceIDs    []string   `json:"DeviceIDs,omitempty"`
	Capabilities [][]string `json:"Capabilities,omitempty"`
}

type portBinding struct {
	HostIP   string `json:"HostIp,omitempty"`
	HostPort string `json:"HostPort,omitempty"` // "" = random
}

// logConfig pins the container log driver instead of inheriting the
// daemon's. Two reasons, both of which bite on a machine that stays up:
//
//   - json-file is unbounded by default, so one chatty service writes
//     until the disk fills. Relay already streams every line to the
//     server, so the container-side copy is only a local tail buffer and
//     a small cap is the right size for it.
//   - a host configured with the "none" driver would otherwise break log
//     streaming entirely, since the agent reads lines back through the
//     Docker logs API.
type logConfig struct {
	Type   string            `json:"Type"`
	Config map[string]string `json:"Config,omitempty"`
}

// cappedLogging keeps at most 30 MiB of container-side log tail per run.
func cappedLogging() logConfig {
	return logConfig{Type: "json-file", Config: map[string]string{
		"max-size": "10m", "max-file": "3",
	}}
}

type containerConfig struct {
	Image        string              `json:"Image"`
	Entrypoint   []string            `json:"Entrypoint,omitempty"`
	Cmd          []string            `json:"Cmd"`
	Env          []string            `json:"Env,omitempty"`
	WorkingDir   string              `json:"WorkingDir,omitempty"`
	Labels       map[string]string   `json:"Labels,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	HostConfig   struct {
		Binds          []string                 `json:"Binds,omitempty"`
		NanoCpus       int64                    `json:"NanoCpus,omitempty"`
		Memory         int64                    `json:"Memory,omitempty"`
		DeviceRequests []deviceRequest          `json:"DeviceRequests,omitempty"`
		PortBindings   map[string][]portBinding `json:"PortBindings,omitempty"`
		AutoRemove     bool                     `json:"AutoRemove"`
		LogConfig      logConfig                `json:"LogConfig"`
	} `json:"HostConfig"`
}

// ListRelayContainers maps container id → run id for every container this
// agent's executors ever labeled (orphan sweep input).
func (c *dockerClient) ListRelayContainers(ctx context.Context) (map[string]string, error) {
	q := url.Values{"all": {"1"},
		"filters": {`{"label":["dev.relay.run"]}`}}
	resp, err := c.do(ctx, http.MethodGet, "/containers/json", q, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.errorFrom(resp)
	}
	var containers []struct {
		Id     string            `json:"Id"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, cont := range containers {
		if runID := cont.Labels["dev.relay.run"]; runID != "" {
			out[cont.Id] = runID
		}
	}
	return out, nil
}

// ContainerHostPort resolves the host port mapped to containerPort/tcp.
func (c *dockerClient) ContainerHostPort(ctx context.Context, id string, containerPort uint32) (string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", c.errorFrom(resp)
	}
	var out struct {
		NetworkSettings struct {
			Ports map[string][]portBinding `json:"Ports"`
		} `json:"NetworkSettings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	key := fmt.Sprintf("%d/tcp", containerPort)
	for _, b := range out.NetworkSettings.Ports[key] {
		if b.HostPort != "" {
			return b.HostPort, nil
		}
	}
	return "", fmt.Errorf("no host port mapped for %s", key)
}

func (c *dockerClient) ContainerCreate(ctx context.Context, name string, cfg *containerConfig) (string, error) {
	q := url.Values{"name": {name}}
	resp, err := c.do(ctx, http.MethodPost, "/containers/create", q, cfg)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusCreated {
		return "", c.errorFrom(resp)
	}
	defer resp.Body.Close()
	var out struct {
		Id string `json:"Id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Id, nil
}

func (c *dockerClient) ContainerStart(ctx context.Context, id string) error {
	resp, err := c.do(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusNoContent {
		return c.errorFrom(resp)
	}
	return nil
}

// ContainerLogs streams demultiplexed log lines. The stream uses Docker's
// 8-byte frame header: [type 0|1|2, 0,0,0, len uint32 BE], then len bytes.
func (c *dockerClient) ContainerLogs(ctx context.Context, id string, onLine func(line string, stderr bool)) error {
	q := url.Values{"follow": {"1"}, "stdout": {"1"}, "stderr": {"1"}}
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+id+"/logs", q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return c.errorFrom(resp)
	}

	r := bufio.NewReaderSize(resp.Body, 64*1024)
	var stdoutBuf, stderrBuf bytes.Buffer
	header := make([]byte, 8)
	flush := func(buf *bytes.Buffer, stderr bool, force bool) {
		for {
			idx := bytes.IndexByte(buf.Bytes(), '\n')
			if idx < 0 {
				break
			}
			onLine(strings.TrimRight(string(buf.Next(idx+1)), "\r\n"), stderr)
		}
		if force && buf.Len() > 0 {
			onLine(buf.String(), stderr)
			buf.Reset()
		}
	}
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			flush(&stdoutBuf, false, true)
			flush(&stderrBuf, true, true)
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		size := binary.BigEndian.Uint32(header[4:8])
		frame := make([]byte, size)
		if _, err := io.ReadFull(r, frame); err != nil {
			return err
		}
		if header[0] == 2 {
			stderrBuf.Write(frame)
			flush(&stderrBuf, true, false)
		} else {
			stdoutBuf.Write(frame)
			flush(&stdoutBuf, false, false)
		}
	}
}

func (c *dockerClient) ContainerWait(ctx context.Context, id string) (int, error) {
	resp, err := c.do(ctx, http.MethodPost, "/containers/"+id+"/wait", nil, nil)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return -1, c.errorFrom(resp)
	}
	var out struct {
		StatusCode int `json:"StatusCode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return -1, err
	}
	return out.StatusCode, nil
}

func (c *dockerClient) ContainerRemove(ctx context.Context, id string) error {
	q := url.Values{"force": {"1"}}
	resp, err := c.do(ctx, http.MethodDelete, "/containers/"+id, q, nil)
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotFound, http.StatusConflict:
		// A gone container, an already removed container, or a removal in progress is fine.
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return nil
	default:
		return c.errorFrom(resp)
	}
}

// waitCtx bounds ContainerWait with a context by polling in a goroutine.
// the wait endpoint itself blocks indefinitely.
func (c *dockerClient) WaitWithTimeout(ctx context.Context, id string, timeout time.Duration) (int, bool, error) {
	waitCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	code, err := c.ContainerWait(waitCtx, id)
	if waitCtx.Err() == context.DeadlineExceeded {
		return -1, true, nil
	}
	return code, false, err
}
