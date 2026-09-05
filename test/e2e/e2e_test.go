package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const portAllocateAttempts = 10

type process struct {
	cmd     *exec.Cmd
	stderr  *bytes.Buffer
	done    chan struct{}
	waitErr error
}

func TestCaddyProcess(t *testing.T) {
	root := repositoryRoot(t)
	template := filepath.Join(root, ".aws-sam", "build", "template.yaml")
	if _, err := os.Stat(template); err != nil {
		t.Skip("run sam build before the E2E test")
	}
	sam, err := exec.LookPath("sam")
	if err != nil {
		t.Skip("SAM CLI is required for the E2E test")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := cleanAWSEnvironment(os.Environ())
	env = append(env,
		"AWS_ACCESS_KEY_ID=test",
		"AWS_SECRET_ACCESS_KEY=test",
		"AWS_SESSION_TOKEN=test",
		"AWS_REGION=us-east-1",
		"AWS_CONFIG_FILE=/dev/null",
		"AWS_SHARED_CREDENTIALS_FILE=/dev/null",
	)

	samPort, samProcess := startServer(t, ctx, "SAM local Lambda", func(port int) (*process, error) {
		return startProcess(ctx, env, sam, "local", "start-lambda",
			"--template", template,
			"--host", "127.0.0.1", "--port", strconv.Itoa(port),
			"--invoke-image", "Lambda=public.ecr.aws/lambda/python:3.12")
	})
	defer func() {
		samProcess.stop()
		if samProcess.waitErr != nil && t.Failed() {
			t.Logf("SAM stderr after test failure: %v\n%s", samProcess.waitErr, samProcess.stderr.String())
		}
	}()

	binary := filepath.Join(t.TempDir(), "caddy")
	build := exec.Command("go", "build", "-o", binary, "./test/e2e/caddy")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test Caddy: %v\n%s", err, output)
	}

	port, process := startServer(t, ctx, "Caddy", func(port int) (*process, error) {
		config := filepath.Join(t.TempDir(), "Caddyfile")
		configContents := fmt.Sprintf(`{
	order lambda before file_server
}

http://127.0.0.1:%d {
	route {
		respond /health 200
		lambda {
			function Lambda
			endpoint http://127.0.0.1:%d
			event_format api_gateway_v2
		timeout 30s
		}
	}
}
`, port, samPort)
		if err := os.WriteFile(config, []byte(configContents), 0o600); err != nil {
			return nil, err
		}
		return startProcess(ctx, env, binary, "run", "--config", config)
	})
	defer func() {
		process.stop()
		if process.waitErr != nil && t.Failed() {
			t.Logf("Caddy stderr after test failure: %v\n%s", process.waitErr, process.stderr.String())
		}
	}()

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 70 * time.Second}
	waitForCaddy(t, client, baseURL+"/health")

	t.Run("GET query headers and body", func(t *testing.T) {
		req := newRequest(t, http.MethodGet, baseURL+"/echo?tag=one&tag=two", nil)
		req.Header.Set("Authorization", "Bearer test")
		req.Header.Set("X-Test", "value")
		req.AddCookie(&http.Cookie{Name: "session", Value: "abc"})
		response := doRequest(t, client, req)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", response.StatusCode)
		}
		var body struct {
			Method  string              `json:"method"`
			Query   map[string][]string `json:"query"`
			Headers map[string]string   `json:"headers"`
		}
		decodeJSON(t, response, &body)
		if body.Method != http.MethodGet || len(body.Query["tag"]) != 2 || body.Query["tag"][0] != "one" || body.Query["tag"][1] != "two" ||
			body.Headers["authorization"] != "Bearer test" || body.Headers["x-test"] != "value" {
			t.Fatalf("unexpected echo response: %#v", body)
		}
	})

	t.Run("POST body", func(t *testing.T) {
		req := newRequest(t, http.MethodPost, baseURL+"/echo", strings.NewReader(`{"hello":"world"}`))
		req.Header.Set("Content-Type", "application/json")
		response := doRequest(t, client, req)
		var body struct {
			Body string `json:"body"`
		}
		decodeJSON(t, response, &body)
		if body.Body != `{"hello":"world"}` {
			t.Fatalf("body = %q", body.Body)
		}
	})

	for _, status := range []int{http.StatusCreated, http.StatusNotFound, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprintf("status %d", status), func(t *testing.T) {
			response := doRequest(t, client, newRequest(t, http.MethodGet, fmt.Sprintf("%s/status/%d", baseURL, status), nil))
			if response.StatusCode != status {
				t.Fatalf("status = %d, want %d", response.StatusCode, status)
			}
			response.Body.Close()
		})
	}

	t.Run("response cookies", func(t *testing.T) {
		response := doRequest(t, client, newRequest(t, http.MethodGet, baseURL+"/cookies", nil))
		cookies := response.Header.Values("Set-Cookie")
		response.Body.Close()
		if len(cookies) != 2 || !strings.Contains(cookies[0], "session=abc") || !strings.Contains(cookies[1], "theme=dark") {
			t.Fatalf("Set-Cookie = %#v", cookies)
		}
	})

	t.Run("binary body", func(t *testing.T) {
		response := doRequest(t, client, newRequest(t, http.MethodGet, baseURL+"/binary", nil))
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "\x00\x01\xfe\xff" || response.Header.Get("Content-Type") != "application/octet-stream" {
			t.Fatalf("body = %v, content-type = %q", body, response.Header.Get("Content-Type"))
		}
	})

	for _, path := range []string{"/timeout", "/lambda-error"} {
		t.Run(path[1:], func(t *testing.T) {
			response := doRequest(t, client, newRequest(t, http.MethodGet, baseURL+path, nil))
			response.Body.Close()
			if response.StatusCode < 500 {
				t.Fatalf("status = %d, want server error", response.StatusCode)
			}
		})
	}
}

func startProcess(ctx context.Context, env []string, name string, args ...string) (*process, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	cmd.Env = env
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(os.Interrupt)
	}
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return &process{stderr: &stderr}, err
	}
	p := &process{cmd: cmd, stderr: &stderr, done: make(chan struct{})}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

func (p *process) stop() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(os.Interrupt)
	}
	<-p.done
}

func startServer(t *testing.T, ctx context.Context, name string, start func(port int) (*process, error)) (int, *process) {
	t.Helper()
	for attempt := 0; attempt < portAllocateAttempts; attempt++ {
		port := freePort(t)
		process, err := start(port)
		if err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
		if waitForPort(process, fmt.Sprintf("127.0.0.1:%d", port)) {
			return port, process
		}
		process.stop()
		if !addressInUse(process.stderr.Bytes()) {
			t.Fatalf("%s did not listen on 127.0.0.1:%d:\n%s", name, port, process.stderr.String())
		}
		t.Logf("port %d is already in use, retrying %s", port, name)
	}
	t.Fatalf("%s could not bind a free port after %d attempts", name, portAllocateAttempts)
	return 0, nil
}

func addressInUse(output []byte) bool {
	return bytes.Contains(output, []byte("address already in use")) ||
		bytes.Contains(output, []byte("EADDRINUSE"))
}

func doRequest(t *testing.T, client *http.Client, request *http.Request) *http.Response {
	t.Helper()
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("request %s: %v", request.URL, err)
	}
	return response
}

func newRequest(t *testing.T, method, url string, body io.Reader) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	return request
}

func cleanAWSEnvironment(environment []string) []string {
	clean := make([]string, 0, len(environment)+8)
	for _, value := range environment {
		key, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(key, "AWS_") {
			continue
		}
		clean = append(clean, value)
	}
	return clean
}

func decodeJSON(t *testing.T, response *http.Response, target any) {
	t.Helper()
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func waitForCaddy(t *testing.T, client *http.Client, url string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(url)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("Caddy did not become ready at %s", url)
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	return root
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate test port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func waitForPort(p *process, address string) bool {
	client := &net.Dialer{Timeout: 100 * time.Millisecond}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			return false
		default:
		}
		connection, err := client.Dial("tcp", address)
		if err == nil {
			connection.Close()
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
