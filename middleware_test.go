package caddylambda

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/smithy-go"
	"github.com/caddyserver/caddy/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type fakeLambdaInvoker struct {
	input  *lambda.InvokeInput
	output *lambda.InvokeOutput
	err    error
	invoke func(context.Context, *lambda.InvokeInput) (*lambda.InvokeOutput, error)
}

func (f *fakeLambdaInvoker) Invoke(ctx context.Context, input *lambda.InvokeInput, _ ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
	f.input = input
	if f.invoke != nil {
		return f.invoke(ctx, input)
	}
	return f.output, f.err
}

func TestInvokeLambdaUsesConfiguredFunctionAndPayload(t *testing.T) {
	fake := &fakeLambdaInvoker{output: &lambda.InvokeOutput{Payload: []byte(`{"ok":true}`)}}
	m := &Lambda{
		FunctionName: "test-function",
		Qualifier:    "prod",
		Timeout:      caddy.Duration(time.Second),
		log:          zap.NewNop(),
		svc:          fake,
	}

	payload, err := m.invokeLambda(context.Background(), Request{Type: "HTTPJSON-REQ"}, "")
	if err != nil {
		t.Fatalf("invokeLambda() error = %v", err)
	}
	if string(payload) != `{"ok":true}` {
		t.Errorf("payload = %q, want %q", payload, `{"ok":true}`)
	}
	if got := *fake.input.FunctionName; got != "test-function" {
		t.Errorf("function name = %q, want %q", got, "test-function")
	}
	if got := *fake.input.Qualifier; got != "prod" {
		t.Errorf("qualifier = %q, want %q", got, "prod")
	}
	var request Request
	if err := json.Unmarshal(fake.input.Payload, &request); err != nil {
		t.Fatalf("decode request payload: %v", err)
	}
	if request.Type != "HTTPJSON-REQ" {
		t.Errorf("request type = %q, want HTTPJSON-REQ", request.Type)
	}
}

func TestInvokeLambdaReturnsFunctionError(t *testing.T) {
	functionError := "Unhandled"
	core, logs := observer.New(zap.DebugLevel)
	fake := &fakeLambdaInvoker{output: &lambda.InvokeOutput{
		FunctionError: &functionError,
		Payload:       []byte("boom"),
	}}
	m := &Lambda{FunctionName: "test-function", Timeout: caddy.Duration(time.Second), log: zap.New(core), svc: fake}

	if _, err := m.invokeLambda(context.Background(), struct{}{}, ""); err == nil {
		t.Fatal("invokeLambda() error = nil, want function error")
	} else if strings.Contains(err.Error(), "boom") {
		t.Fatalf("invokeLambda() error = %v, must not include function payload", err)
	}
	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("log entries = %d, want 1", len(entries))
	}
	if got := entries[0].ContextMap()["error"]; got == nil || !strings.Contains(got.(string), "Unhandled") {
		t.Fatalf("logged error = %#v, want Unhandled", got)
	}
}

func TestInvokeLambdaLogsSafeDebugFields(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	fake := &fakeLambdaInvoker{output: &lambda.InvokeOutput{Payload: []byte(`{}`)}}
	m := &Lambda{
		FunctionName: "test-function",
		Qualifier:    "prod",
		Timeout:      caddy.Duration(time.Second),
		log:          zap.New(core),
		svc:          fake,
	}

	if _, err := m.invokeLambda(context.Background(), struct{}{}, "request-123"); err != nil {
		t.Fatalf("invokeLambda() error = %v", err)
	}

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("log entries = %d, want 1", len(entries))
	}
	fields := entries[0].ContextMap()
	if fields["function"] != "test-function" || fields["request_id"] != "request-123" {
		t.Fatalf("log fields = %#v, want function and request_id", fields)
	}
	if _, ok := fields["duration"]; !ok {
		t.Fatal("log fields missing duration")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name          string
		middleware    Lambda
		wantErrorText string
	}{
		{name: "missing function", wantErrorText: "function must be configured"},
		{
			name:          "unknown event format",
			middleware:    Lambda{FunctionName: "test-function", EventFormat: "unknown"},
			wantErrorText: "unsupported event format \"unknown\"",
		},
		{
			name:       "default event format",
			middleware: Lambda{FunctionName: "test-function"},
		},
		{
			name:       "api gateway v2",
			middleware: Lambda{FunctionName: "test-function", EventFormat: eventFormatAPIGatewayV2},
		},
		{
			name:          "negative max body size",
			middleware:    Lambda{FunctionName: "test-function", MaxBodySize: -1},
			wantErrorText: "max_body_size must not be negative",
		},
		{
			name:          "role options require role",
			middleware:    Lambda{FunctionName: "test-function", ExternalID: "external-test"},
			wantErrorText: "external_id and session_name require role_arn",
		},
		{
			name:          "negative timeout",
			middleware:    Lambda{FunctionName: "test-function", Timeout: -1},
			wantErrorText: "timeout must not be negative",
		},
		{
			name:          "timeout exceeds limit",
			middleware:    Lambda{FunctionName: "test-function", Timeout: caddy.Duration(maxLambdaTimeout + time.Second)},
			wantErrorText: "timeout exceeds Lambda's 15-minute execution limit",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.middleware.Validate()
			if test.wantErrorText == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || err.Error() != test.wantErrorText {
				t.Fatalf("Validate() error = %v, want %q", err, test.wantErrorText)
			}
		})
	}
}

func TestIsInsecureEndpoint(t *testing.T) {
	for endpoint, want := range map[string]bool{
		"":                       false,
		"http://127.0.0.1:3001":  true,
		"HTTP://127.0.0.1:3001":  true,
		"https://lambda.example": false,
		"lambda.example":         false,
	} {
		if got := isInsecureEndpoint(endpoint); got != want {
			t.Errorf("isInsecureEndpoint(%q) = %v, want %v", endpoint, got, want)
		}
	}
}

func TestServeHTTPRejectsInvalidBase64BeforeWriting(t *testing.T) {
	fake := &fakeLambdaInvoker{output: &lambda.InvokeOutput{Payload: []byte(`{
		"type":"HTTPJSON-REP",
		"meta":{"status":200},
		"body":"not-base64",
		"bodyEncoding":"base64"
	}`)}}
	m := &Lambda{FunctionName: "test-function", EventFormat: eventFormatHTTPJSON, Timeout: caddy.Duration(time.Second), log: zap.NewNop(), svc: fake}
	w := httptest.NewRecorder()

	err := m.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil), nil)
	if err == nil {
		t.Fatal("ServeHTTP() error = nil, want base64 decoding error")
	}
	if len(w.Header()) != 0 || w.Body.Len() != 0 {
		t.Fatalf("response headers/body = %#v/%q, want no response data before write", w.Header(), w.Body.String())
	}
}

func TestServeHTTPReturnsApplicationResponse(t *testing.T) {
	fake := &fakeLambdaInvoker{output: &lambda.InvokeOutput{Payload: []byte(`{
		"type":"HTTPJSON-REP",
		"meta":{"status":201,"headers":{"x-test":["one","two"]}},
		"body":"AQI=",
		"bodyEncoding":"base64"
	}`)}}
	m := &Lambda{FunctionName: "test-function", EventFormat: eventFormatHTTPJSON, Timeout: caddy.Duration(time.Second), log: zap.NewNop(), svc: fake}
	w := httptest.NewRecorder()

	if err := m.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil), nil); err != nil {
		t.Fatalf("ServeHTTP() error = %v", err)
	}
	if w.Code != http.StatusCreated || string(w.Body.Bytes()) != "\x01\x02" {
		t.Fatalf("response = %d/%v, want 201/[1 2]", w.Code, w.Body.Bytes())
	}
	if got := w.Header().Values("X-Test"); len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Errorf("X-Test = %#v, want [one two]", got)
	}
}

func TestServeHTTPGeneratesRequestIDWhenAbsent(t *testing.T) {
	fake := &fakeLambdaInvoker{output: &lambda.InvokeOutput{Payload: []byte(`{"statusCode":200,"body":"ok"}`)}}
	m := &Lambda{FunctionName: "test-function", EventFormat: eventFormatAPIGatewayV2, Timeout: caddy.Duration(time.Second), log: zap.NewNop(), svc: fake}
	w := httptest.NewRecorder()

	if err := m.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil), nil); err != nil {
		t.Fatalf("ServeHTTP() error = %v", err)
	}

	var event APIGatewayV2Request
	if err := json.Unmarshal(fake.input.Payload, &event); err != nil {
		t.Fatalf("decode request payload: %v", err)
	}
	requestID := event.RequestContext.RequestID
	if requestID == "" {
		t.Fatal("request ID is empty, want generated ID")
	}
	if _, err := uuid.Parse(requestID); err != nil {
		t.Fatalf("request ID %q is not a UUID: %v", requestID, err)
	}
	if got := w.Header().Get("X-Request-ID"); got != requestID {
		t.Errorf("X-Request-ID response header = %q, want %q", got, requestID)
	}
}

func TestServeHTTPPreservesExistingRequestID(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	fake := &fakeLambdaInvoker{output: &lambda.InvokeOutput{Payload: []byte(`{"statusCode":200,"body":"ok"}`)}}
	m := &Lambda{FunctionName: "test-function", EventFormat: eventFormatAPIGatewayV2, Timeout: caddy.Duration(time.Second), log: zap.New(core), svc: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Request-ID", "existing-id")

	if err := m.ServeHTTP(w, r, nil); err != nil {
		t.Fatalf("ServeHTTP() error = %v", err)
	}

	var event APIGatewayV2Request
	if err := json.Unmarshal(fake.input.Payload, &event); err != nil {
		t.Fatalf("decode request payload: %v", err)
	}
	if got := event.RequestContext.RequestID; got != "existing-id" {
		t.Errorf("request ID = %q, want %q", got, "existing-id")
	}
	if got := w.Header().Get("X-Request-ID"); got != "existing-id" {
		t.Errorf("X-Request-ID response header = %q, want %q", got, "existing-id")
	}
	if got := logs.All()[0].ContextMap()["request_id"]; got != "existing-id" {
		t.Errorf("logged request_id = %#v, want existing-id", got)
	}
}

func TestServeHTTPDoesNotGenerateRequestIDForHTTPJSON(t *testing.T) {
	fake := &fakeLambdaInvoker{output: &lambda.InvokeOutput{Payload: []byte(`{
		"type":"HTTPJSON-REP",
		"meta":{"status":200},
		"body":"ok"
	}`)}}
	m := &Lambda{FunctionName: "test-function", EventFormat: eventFormatHTTPJSON, Timeout: caddy.Duration(time.Second), log: zap.NewNop(), svc: fake}
	w := httptest.NewRecorder()

	if err := m.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil), nil); err != nil {
		t.Fatalf("ServeHTTP() error = %v", err)
	}

	var request Request
	if err := json.Unmarshal(fake.input.Payload, &request); err != nil {
		t.Fatalf("decode request payload: %v", err)
	}
	if _, ok := request.Meta.Headers["x-request-id"]; ok {
		t.Fatal("HTTPJSON request contains a synthetic x-request-id")
	}
	if got := w.Header().Get("X-Request-ID"); got != "" {
		t.Errorf("X-Request-ID response header = %q, want empty", got)
	}
}

func TestServeHTTPLimitsLoggedRequestID(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	fake := &fakeLambdaInvoker{output: &lambda.InvokeOutput{Payload: []byte(`{"type":"HTTPJSON-REP","meta":{"status":200},"body":"ok"}`)}}
	m := &Lambda{FunctionName: "test-function", EventFormat: eventFormatHTTPJSON, Timeout: caddy.Duration(time.Second), log: zap.New(core), svc: fake}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Request-ID", strings.Repeat("x", maxLoggedRequestIDLength+1))

	if err := m.ServeHTTP(httptest.NewRecorder(), r, nil); err != nil {
		t.Fatalf("ServeHTTP() error = %v", err)
	}
	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("log entries = %d, want 1", len(entries))
	}
	if got := entries[0].ContextMap()["request_id"].(string); len(got) != maxLoggedRequestIDLength {
		t.Errorf("logged request ID length = %d, want %d", len(got), maxLoggedRequestIDLength)
	}
}

func TestServeHTTPSkipsBodyForNoBodyStatusesAndFramingHeaders(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusNotModified} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			fake := &fakeLambdaInvoker{output: &lambda.InvokeOutput{Payload: []byte(`{
				"type":"HTTPJSON-REP",
				"meta":{"status":` + strconv.Itoa(status) + `,"headers":{"Content-Length":["999"],"Transfer-Encoding":["chunked"],"Connection":["close"]}},
				"body":"must not be written"
			}`)}}
			m := &Lambda{FunctionName: "test-function", EventFormat: eventFormatHTTPJSON, Timeout: caddy.Duration(time.Second), log: zap.NewNop(), svc: fake}
			w := httptest.NewRecorder()

			if err := m.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil), nil); err != nil {
				t.Fatalf("ServeHTTP() error = %v", err)
			}
			if w.Code != status || w.Body.Len() != 0 {
				t.Fatalf("response = %d/%q, want %d/empty", w.Code, w.Body.String(), status)
			}
			for _, header := range []string{"Content-Length", "Transfer-Encoding", "Connection"} {
				if got := w.Header().Get(header); got != "" {
					t.Errorf("%s = %q, want empty", header, got)
				}
			}
		})
	}
}

func TestServeHTTPStripsHopByHopHeaders(t *testing.T) {
	fake := &fakeLambdaInvoker{output: &lambda.InvokeOutput{Payload: []byte(`{
		"type":"HTTPJSON-REP",
		"meta":{"status":200,"headers":{
			"Connection":["keep-alive, X-Nominated"],
			"Proxy-Connection":["keep-alive"],
			"Keep-Alive":["timeout=5"],
			"Proxy-Authenticate":["Basic"],
			"Proxy-Authorization":["Basic dXNlcjpwYXNz"],
			"TE":["trailers"],
			"Trailer":["X-Trailer"],
			"Transfer-Encoding":["chunked"],
			"Upgrade":["websocket"],
			"X-Nominated":["strip-me"],
			"Content-Type":["text/plain"],
			"X-Custom":["keep"]
		}},
		"body":"ok"
	}`)}}
	m := &Lambda{FunctionName: "test-function", EventFormat: eventFormatHTTPJSON, Timeout: caddy.Duration(time.Second), log: zap.NewNop(), svc: fake}
	w := httptest.NewRecorder()

	if err := m.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil), nil); err != nil {
		t.Fatalf("ServeHTTP() error = %v", err)
	}

	for _, header := range []string{
		"Connection",
		"Proxy-Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"TE",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
		"Content-Length",
		"X-Nominated",
	} {
		if got := w.Header().Get(header); got != "" {
			t.Errorf("%s = %q, want empty", header, got)
		}
	}

	if got := w.Header().Get("Content-Type"); got != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain", got)
	}
	if got := w.Header().Get("X-Custom"); got != "keep" {
		t.Errorf("X-Custom = %q, want keep", got)
	}
}

func TestServeHTTPReturnsApplicationErrorStatus(t *testing.T) {
	fake := &fakeLambdaInvoker{output: &lambda.InvokeOutput{Payload: []byte(`{
		"type":"HTTPJSON-REP",
		"meta":{"status":503},
		"body":"unavailable"
	}`)}}
	m := &Lambda{FunctionName: "test-function", EventFormat: eventFormatHTTPJSON, Timeout: caddy.Duration(time.Second), log: zap.NewNop(), svc: fake}
	w := httptest.NewRecorder()

	if err := m.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil), nil); err != nil {
		t.Fatalf("ServeHTTP() error = %v, want nil for application error", err)
	}
	if w.Code != http.StatusServiceUnavailable || w.Body.String() != "unavailable" {
		t.Fatalf("response = %d/%q, want 503/unavailable", w.Code, w.Body.String())
	}
}

func TestInvokeLambdaReturnsTimeout(t *testing.T) {
	fake := &fakeLambdaInvoker{invoke: func(ctx context.Context, _ *lambda.InvokeInput) (*lambda.InvokeOutput, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	m := &Lambda{FunctionName: "test-function", Timeout: caddy.Duration(time.Millisecond), log: zap.NewNop(), svc: fake}

	if _, err := m.invokeLambda(context.Background(), struct{}{}, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("invokeLambda() error = %v, want deadline exceeded", err)
	}
}

func TestInvokeLambdaReturnsThrottlingError(t *testing.T) {
	wantErr := errors.New("throttled")
	fake := &fakeLambdaInvoker{err: wantErr}
	m := &Lambda{FunctionName: "test-function", Timeout: caddy.Duration(time.Second), log: zap.NewNop(), svc: fake}

	if _, err := m.invokeLambda(context.Background(), struct{}{}, ""); !errors.Is(err, wantErr) {
		t.Fatalf("invokeLambda() error = %v, want throttling error", err)
	}
}

func TestLambdaErrorStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "timeout", err: context.DeadlineExceeded, want: http.StatusGatewayTimeout},
		{
			name: "throttled",
			err:  &smithy.GenericAPIError{Code: "TooManyRequestsException"},
			want: http.StatusServiceUnavailable,
		},
		{name: "invoke failure", err: errors.New("invoke failed"), want: http.StatusBadGateway},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := lambdaErrorStatus(test.err); got != test.want {
				t.Errorf("lambdaErrorStatus() = %d, want %d", got, test.want)
			}
		})
	}
}
