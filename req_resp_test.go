package caddylambda

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func TestNewRequestForFormatAPIGatewayV2(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "http://example.test/items?tag=one&tag=two", strings.NewReader("\x00\x01"))
	r.RemoteAddr = "192.0.2.1:1234"
	r.Header.Add("X-Test", "one")
	r.Header.Add("X-Test", "two")
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Header.Set("X-Request-ID", "request-123")
	r.Header.Set("Cookie", "session=a b; theme=dark")

	payload, err := newRequestForFormat(httptest.NewRecorder(), r, eventFormatAPIGatewayV2, 0, nil)
	if err != nil {
		t.Fatalf("newRequestForFormat() error = %v", err)
	}

	event, ok := payload.(*APIGatewayV2Request)
	if !ok {
		t.Fatalf("payload type = %T, want *APIGatewayV2Request", payload)
	}
	if event.Version != "2.0" {
		t.Errorf("Version = %q, want %q", event.Version, "2.0")
	}
	if event.RawPath != "/items" || event.RawQueryString != "tag=one&tag=two" {
		t.Errorf("path = %q?%q, want /items?tag=one&tag=two", event.RawPath, event.RawQueryString)
	}
	if event.Headers["x-test"] != "one,two" {
		t.Errorf("x-test = %q, want %q", event.Headers["x-test"], "one,two")
	}
	if event.QueryStringParameters["tag"] != "one,two" {
		t.Errorf("tag = %q, want %q", event.QueryStringParameters["tag"], "one,two")
	}
	if got, want := strings.Join(event.Cookies, ","), "session=a b,theme=dark"; got != want {
		t.Errorf("Cookies = %#v, want [%s]", event.Cookies, strings.ReplaceAll(want, ",", " "))
	}
	if event.RequestContext.HTTP.SourceIP != "192.0.2.1" {
		t.Errorf("SourceIP = %q, want %q", event.RequestContext.HTTP.SourceIP, "192.0.2.1")
	}
	if event.Headers["x-forwarded-proto"] != "http" || event.Headers["x-forwarded-for"] != "192.0.2.1" || event.Headers["x-forwarded-port"] != "80" {
		t.Errorf("forwarded headers = %#v, want http/192.0.2.1/80", event.Headers)
	}
	if event.RequestContext.RequestID != "request-123" || event.RequestContext.DomainName != "example.test" || event.RequestContext.Stage != "$default" || event.RequestContext.TimeEpoch <= 0 {
		t.Errorf("request context = %#v, want populated API Gateway context", event.RequestContext)
	}
	if event.Body != base64.StdEncoding.EncodeToString([]byte("\x00\x01")) || !event.IsBase64Encoded {
		t.Errorf("binary body = %q, encoded = %v", event.Body, event.IsBase64Encoded)
	}
}

func TestRequestSourceIPUsesCaddyClientIP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.1:1234"
	r = r.WithContext(context.WithValue(r.Context(), caddyhttp.VarsCtxKey, map[string]any{
		caddyhttp.ClientIPVarKey: "198.51.100.7",
	}))

	if got := requestSourceIP(r); got != "198.51.100.7" {
		t.Errorf("requestSourceIP() = %q, want %q", got, "198.51.100.7")
	}
}

func TestNewRequestForFormatDefaultsToAPIGatewayV2(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", strings.NewReader("body"))
	m := &Lambda{}

	payload, err := newRequestForFormat(httptest.NewRecorder(), r, m.eventFormat(), 0, nil)
	if err != nil {
		t.Fatalf("newRequestForFormat() error = %v", err)
	}

	request, ok := payload.(*APIGatewayV2Request)
	if !ok {
		t.Fatalf("payload type = %T, want *APIGatewayV2Request", payload)
	}
	if request.Version != "2.0" || request.Body != "body" || request.IsBase64Encoded {
		t.Errorf("request = %#v, want API Gateway v2 with body", request)
	}
}

func TestNewRequestForFormatHTTPJSONEncodesBinaryBody(t *testing.T) {
	body := []byte{0x00, 0xff, 0x01}
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/octet-stream")

	payload, err := newRequestForFormat(httptest.NewRecorder(), r, eventFormatHTTPJSON, 0, nil)
	if err != nil {
		t.Fatalf("newRequestForFormat() error = %v", err)
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var request Request
	if err := json.Unmarshal(encoded, &request); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if request.Body != base64.StdEncoding.EncodeToString(body) || request.BodyEncoding != "base64" {
		t.Errorf("request = %#v, want base64-encoded body", request)
	}
}

func TestRequestBodyEncodingRecognizesTextualMediaTypes(t *testing.T) {
	for _, contentType := range []string{
		"text/plain",
		"application/json; charset=utf-8",
		"application/vnd.api+json",
		"application/problem+json",
		"application/x-ndjson",
		"application/graphql",
		"application/xml",
	} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("textual body"))
		r.Header.Set("Content-Type", contentType)
		if requestBodyIsBase64Encoded(r, []byte("textual body")) {
			t.Errorf("requestBodyIsBase64Encoded() = true for %q", contentType)
		}
	}
}

func TestNewRequestForFormatAppliesUpstreamHeaders(t *testing.T) {
	replacer := caddy.NewReplacer()
	replacer.Set("http.request.host", "example.test")
	r := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	r.Header.Set("X-Test", "client")
	r = r.WithContext(context.WithValue(r.Context(), caddy.ReplacerCtxKey, replacer))
	configured := map[string][]string{
		"X-Test": {"one", "{http.request.host}"},
	}

	for _, format := range []string{eventFormatHTTPJSON, eventFormatAPIGatewayV2} {
		payload, err := newRequestForFormat(httptest.NewRecorder(), r, format, 0, configured)
		if err != nil {
			t.Fatalf("newRequestForFormat(%q) error = %v", format, err)
		}
		switch request := payload.(type) {
		case *Request:
			if got := request.Meta.Headers["x-test"]; len(got) != 2 || got[1] != "example.test" {
				t.Errorf("HTTPJSON headers = %#v, want configured values", got)
			}
		case *APIGatewayV2Request:
			if got := request.Headers["x-test"]; got != "one,example.test" {
				t.Errorf("API Gateway v2 headers = %q, want one,example.test", got)
			}
		}
	}
}

func TestNewRequestForFormatRejectsUnknownFormat(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, err := newRequestForFormat(httptest.NewRecorder(), r, "unknown", 0, nil); err == nil {
		t.Fatal("newRequestForFormat() error = nil, want error")
	}
}

func TestNewRequestForFormatRejectsOversizedBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("12345"))
	_, err := newRequestForFormat(httptest.NewRecorder(), r, eventFormatHTTPJSON, 4, nil)
	if err == nil {
		t.Fatal("newRequestForFormat() error = nil, want body size error")
	}
	if got := err.Error(); !strings.Contains(got, "request body exceeds maximum size of 4 bytes") {
		t.Fatalf("error = %q, want body size error", got)
	}
}

func TestReadRequestBodyUsesDefaultLimit(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", defaultMaxBodySize+1)))

	_, err := readRequestBody(httptest.NewRecorder(), r, 0)
	if err == nil || !strings.Contains(err.Error(), "request body exceeds maximum size of 4194304 bytes") {
		t.Fatalf("readRequestBody() error = %v, want default body size error", err)
	}
}

func TestParseReplyRejectsInvalidAPIGatewayV2Status(t *testing.T) {
	_, err := parseReply([]byte(`{"statusCode": 700}`), eventFormatAPIGatewayV2)
	if err == nil || !strings.Contains(err.Error(), "invalid statusCode 700") {
		t.Fatalf("parseReply() error = %v, want invalid status error", err)
	}
}

func TestParseReplyHTTPJSON(t *testing.T) {
	reply, err := parseReply([]byte(`{"type":"HTTPJSON-REP","meta":{"status":201,"headers":{"x-test":["one","two"]}},"body":"created"}`), eventFormatHTTPJSON)
	if err != nil {
		t.Fatalf("parseReply() error = %v", err)
	}
	if reply.Meta.Status != http.StatusCreated || reply.Body != "created" {
		t.Fatalf("reply = %#v, want status 201 and created body", reply)
	}
	if got := reply.Meta.Headers["x-test"]; len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Errorf("headers = %#v, want [one two]", got)
	}
}

func TestParseReplyRejectsMalformedHTTPJSONEnvelope(t *testing.T) {
	_, err := parseReply([]byte(`{"type":"HTTPJSON-REP","meta":{"headers":{"x-test":"scalar"}}}`), eventFormatHTTPJSON)
	if err == nil || !strings.Contains(err.Error(), "decode HTTPJSON response") {
		t.Fatalf("parseReply() error = %v, want HTTPJSON envelope error", err)
	}
}

func TestParseReplyAPIGatewayV2(t *testing.T) {
	reply, err := parseReply([]byte(`{"statusCode":202,"headers":{"content-type":"text/plain"},"cookies":["one=1","two=2"],"body":"AQI=","isBase64Encoded":true}`), eventFormatAPIGatewayV2)
	if err != nil {
		t.Fatalf("parseReply() error = %v", err)
	}
	if reply.Meta.Status != http.StatusAccepted || reply.Body != "AQI=" || reply.BodyEncoding != "base64" {
		t.Fatalf("reply = %#v, want API Gateway response fields", reply)
	}
	if got := reply.Meta.Headers["set-cookie"]; len(got) != 2 || got[0] != "one=1" || got[1] != "two=2" {
		t.Errorf("cookies = %#v, want [one=1 two=2]", got)
	}
}

func TestParseReplyRejectsMalformedAPIGatewayV2JSON(t *testing.T) {
	if _, err := parseReply([]byte(`{"statusCode":`), eventFormatAPIGatewayV2); err == nil {
		t.Fatal("parseReply() error = nil, want malformed JSON error")
	}
}

func TestParseReplyTreatsMalformedHTTPJSONAsBody(t *testing.T) {
	reply, err := parseReply([]byte(`{"type":`), eventFormatHTTPJSON)
	if err != nil {
		t.Fatalf("parseReply() error = %v", err)
	}
	if reply.Body != `{"type":` || reply.Meta.Status != http.StatusOK {
		t.Fatalf("reply = %#v, want malformed input as 200 body", reply)
	}
}

func TestValidateReplyRejectsInvalidHTTPJSONResponse(t *testing.T) {
	reply := &Reply{Type: "HTTPJSON-REP", Meta: &ReplyMeta{Status: 700}}
	if err := validateReply(reply); err == nil || !strings.Contains(err.Error(), "invalid status 700") {
		t.Fatalf("validateReply() error = %v, want invalid status error", err)
	}
}

func TestValidateReplyRejectsUnknownBodyEncoding(t *testing.T) {
	reply := &Reply{Meta: &ReplyMeta{}, BodyEncoding: "base64url"}
	if err := validateReply(reply); err == nil || !strings.Contains(err.Error(), "unsupported body encoding") {
		t.Fatalf("validateReply() error = %v, want encoding error", err)
	}
}

func TestValidateReplyRejectsMissingMetadata(t *testing.T) {
	if err := validateReply(&Reply{}); err == nil || !strings.Contains(err.Error(), "missing metadata") {
		t.Fatalf("validateReply() error = %v, want missing metadata error", err)
	}
}

func TestReadRequestBodyDoesNotCloseBody(t *testing.T) {
	body := &trackingBody{Reader: strings.NewReader("body")}
	r := httptest.NewRequest(http.MethodPost, "/", body)

	if _, err := readRequestBody(httptest.NewRecorder(), r, 0); err != nil {
		t.Fatalf("readRequestBody() error = %v", err)
	}
	if body.closed {
		t.Fatal("readRequestBody() closed the request body")
	}
}

type trackingBody struct {
	io.Reader
	closed bool
}

func (b *trackingBody) Close() error {
	b.closed = true
	return nil
}
