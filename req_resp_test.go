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
)

func TestNewRequestForFormatAPIGatewayV2(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "http://example.test/items?tag=one&tag=two", strings.NewReader("\x00\x01"))
	r.RemoteAddr = "192.0.2.1:1234"
	r.Header.Add("X-Test", "one")
	r.Header.Add("X-Test", "two")
	r.Header.Set("Content-Type", "application/octet-stream")
	r.AddCookie(&http.Cookie{Name: "session", Value: "abc"})

	payload, err := newRequestForFormat(r, eventFormatAPIGatewayV2, 0, nil)
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
	if len(event.Cookies) != 1 || event.Cookies[0] != "session=abc" {
		t.Errorf("Cookies = %#v, want [session=abc]", event.Cookies)
	}
	if event.RequestContext.HTTP.SourceIP != "192.0.2.1" {
		t.Errorf("SourceIP = %q, want %q", event.RequestContext.HTTP.SourceIP, "192.0.2.1")
	}
	if event.Body != base64.StdEncoding.EncodeToString([]byte("\x00\x01")) || !event.IsBase64Encoded {
		t.Errorf("binary body = %q, encoded = %v", event.Body, event.IsBase64Encoded)
	}
}

func TestNewRequestForFormatDefaultsToHTTPJSON(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", strings.NewReader("body"))
	m := &Lambda{}

	payload, err := newRequestForFormat(r, m.eventFormat(), 0, nil)
	if err != nil {
		t.Fatalf("newRequestForFormat() error = %v", err)
	}

	request, ok := payload.(*Request)
	if !ok {
		t.Fatalf("payload type = %T, want *Request", payload)
	}
	if request.Type != "HTTPJSON-REQ" || request.Body != "body" {
		t.Errorf("request = %#v, want HTTPJSON-REQ with body", request)
	}
}

func TestNewRequestForFormatHTTPJSONEncodesBinaryBody(t *testing.T) {
	body := []byte{0x00, 0xff, 0x01}
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/octet-stream")

	payload, err := newRequestForFormat(r, eventFormatHTTPJSON, 0, nil)
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
		payload, err := newRequestForFormat(r, format, 0, configured)
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
	if _, err := newRequestForFormat(r, "unknown", 0, nil); err == nil {
		t.Fatal("newRequestForFormat() error = nil, want error")
	}
}

func TestNewRequestForFormatRejectsOversizedBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("12345"))
	_, err := newRequestForFormat(r, eventFormatHTTPJSON, 4, nil)
	if err == nil {
		t.Fatal("newRequestForFormat() error = nil, want body size error")
	}
	if got := err.Error(); !strings.Contains(got, "request body exceeds maximum size of 4 bytes") {
		t.Fatalf("error = %q, want body size error", got)
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

	if _, err := readRequestBody(r, 0); err != nil {
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
