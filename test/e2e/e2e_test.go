package e2e

import (
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

	samPort := freePort(t)
	samProcess := exec.CommandContext(ctx, sam, "local", "start-lambda",
		"--template", template,
		"--host", "127.0.0.1", "--port", strconv.Itoa(samPort))
	samProcess.Args = append(samProcess.Args, "--invoke-image", "Lambda=public.ecr.aws/lambda/python:3.12")
	samProcess.Env = env
	samProcess.Stdout = io.Discard
	samProcess.Stderr = io.Discard
	if err := samProcess.Start(); err != nil {
		t.Fatalf("start SAM local Lambda: %v", err)
	}
	defer func() { cancel(); _ = samProcess.Wait() }()
	waitForPort(t, fmt.Sprintf("127.0.0.1:%d", samPort))

	binary := filepath.Join(t.TempDir(), "caddy")
	build := exec.Command("go", "build", "-o", binary, "./test/e2e/caddy")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test Caddy: %v\n%s", err, output)
	}

	port := freePort(t)
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
		t.Fatalf("write Caddyfile: %v", err)
	}

	process := exec.CommandContext(ctx, binary, "run", "--config", config)
	process.Env = env
	process.Stdout = io.Discard
	process.Stderr = io.Discard
	if err := process.Start(); err != nil {
		t.Fatalf("start Caddy: %v", err)
	}
	defer func() { cancel(); _ = process.Wait() }()

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
		switch key {
		case "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_REGION",
			"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE":
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

func waitForPort(t *testing.T, address string) {
	t.Helper()
	client := &net.Dialer{Timeout: 100 * time.Millisecond}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := client.Dial("tcp", address)
		if err == nil {
			connection.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process did not listen at %s", address)
}
