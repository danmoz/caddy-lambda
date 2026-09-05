package caddylambda

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

const (
	eventFormatHTTPJSON     = "httpjson"
	eventFormatAPIGatewayV2 = "api_gateway_v2"
	defaultMaxBodySize      = 4 * 1024 * 1024
)

// parseReply unpacks the Lambda response data into a Reply.
// If the reply is a JSON object with a 'type' field equal to 'HTTPJSON-REP', then
// data will be unmarshaled directly as a Reply struct.
//
// If data is not a JSON object, or the object's type field is omitted or set to
// a string other than 'HTTPJSON-REP', then data will be set as the Reply.body
// and Reply.meta will contain a default struct with a 200 status and
// a content-type header of 'application/json'.
func parseReply(data []byte, format string) (*Reply, error) {
	if format == eventFormatAPIGatewayV2 {
		var response APIGatewayV2Response
		if err := json.Unmarshal(data, &response); err != nil {
			return nil, fmt.Errorf("decode API Gateway v2 response: %w", err)
		}
		if response.StatusCode == 0 {
			return nil, fmt.Errorf("API Gateway v2 response is missing statusCode")
		}
		if response.StatusCode < 100 || response.StatusCode > 599 {
			return nil, fmt.Errorf("API Gateway v2 response has invalid statusCode %d", response.StatusCode)
		}
		headers := make(map[string][]string, len(response.Headers))
		for key, value := range response.Headers {
			headers[key] = []string{value}
		}
		if len(response.Cookies) > 0 {
			headers["set-cookie"] = append(headers["set-cookie"], response.Cookies...)
		}
		return &Reply{
			Type:         "HTTPJSON-REP",
			Meta:         &ReplyMeta{Status: response.StatusCode, Headers: headers},
			Body:         response.Body,
			BodyEncoding: boolEncoding(response.IsBase64Encoded),
		}, nil
	}

	if len(data) > 0 && data[0] == '{' {
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &envelope); err == nil && envelope.Type == "HTTPJSON-REP" {
			var rep Reply
			if err := json.Unmarshal(data, &rep); err != nil {
				return nil, fmt.Errorf("decode HTTPJSON response: %w", err)
			}
			if rep.Meta == nil {
				rep.Meta = defaultReplyMeta()
			}
			return &rep, nil
		}
	}

	return &Reply{
		Type: "HTTPJSON-REP",
		Meta: defaultReplyMeta(),
		Body: string(data),
	}, nil
}

func defaultReplyMeta() *ReplyMeta {
	return &ReplyMeta{
		Status:  http.StatusOK,
		Headers: map[string][]string{"content-type": {"application/json"}},
	}
}

func validateReply(reply *Reply) error {
	if reply == nil {
		return fmt.Errorf("response is empty")
	}
	if reply.Meta == nil {
		return fmt.Errorf("response is missing metadata")
	}
	if reply.Meta.Status != 0 && (reply.Meta.Status < 100 || reply.Meta.Status > 599) {
		return fmt.Errorf("response has invalid status %d", reply.Meta.Status)
	}
	if reply.BodyEncoding != "" && reply.BodyEncoding != "base64" {
		return fmt.Errorf("response has unsupported body encoding %q", reply.BodyEncoding)
	}
	return nil
}

func boolEncoding(encoded bool) string {
	if encoded {
		return "base64"
	}
	return ""
}

func newRequestForFormat(w http.ResponseWriter, r *http.Request, format string, maxBodySize int64, upstreamHeaders map[string][]string) (any, error) {
	body, err := readRequestBody(w, r, maxBodySize)
	if err != nil {
		return nil, err
	}

	var request any
	switch format {
	case eventFormatHTTPJSON:
		request = newHTTPJSONRequest(r, body)
	case eventFormatAPIGatewayV2:
		request = newAPIGatewayV2Request(r, body)
	default:
		return nil, fmt.Errorf("unsupported event format %q", format)
	}
	applyUpstreamHeaders(r, request, upstreamHeaders)
	return request, nil
}

func applyUpstreamHeaders(r *http.Request, request any, upstreamHeaders map[string][]string) {
	replacer, _ := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	for key, configuredValues := range upstreamHeaders {
		key = strings.ToLower(key)
		values := make([]string, len(configuredValues))
		for i, value := range configuredValues {
			if replacer != nil {
				value = replacer.ReplaceAll(value, "")
			}
			values[i] = value
		}
		switch request := request.(type) {
		case *Request:
			request.Meta.Headers[key] = append([]string(nil), values...)
		case *APIGatewayV2Request:
			request.Headers[key] = strings.Join(values, ",")
		}
	}
}

func readRequestBody(w http.ResponseWriter, r *http.Request, maxBodySize int64) ([]byte, error) {
	if maxBodySize <= 0 {
		maxBodySize = defaultMaxBodySize
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			return nil, caddyhttp.Error(http.StatusRequestEntityTooLarge,
				fmt.Errorf("request body exceeds maximum size of %d bytes", maxBodySize))
		}
	}
	return body, err
}

func newHTTPJSONRequest(r *http.Request, body []byte) *Request {
	encoded := requestBodyIsBase64Encoded(r, body)
	if encoded {
		body = []byte(base64.StdEncoding.EncodeToString(body))
	}
	return &Request{
		Type:         "HTTPJSON-REQ",
		Meta:         newRequestMeta(r),
		Body:         string(body),
		BodyEncoding: boolEncoding(encoded),
	}
}

func newAPIGatewayV2Request(r *http.Request, body []byte) *APIGatewayV2Request {
	encoded := requestBodyIsBase64Encoded(r, body)
	requestBody := string(body)
	if encoded {
		requestBody = base64.StdEncoding.EncodeToString(body)
	}
	return &APIGatewayV2Request{
		Version:               "2.0",
		RouteKey:              "$default",
		RawPath:               r.URL.EscapedPath(),
		RawQueryString:        r.URL.RawQuery,
		Headers:               apiGatewayV2Headers(r),
		Cookies:               apiGatewayV2Cookies(r),
		QueryStringParameters: apiGatewayV2QueryParameters(r),
		RequestContext: APIGatewayV2RequestContext{
			RequestID:  r.Header.Get("X-Request-ID"),
			TimeEpoch:  time.Now().UnixMilli(),
			DomainName: requestDomainName(r),
			Stage:      "$default",
			HTTP: APIGatewayV2HTTPContext{
				Method:    r.Method,
				Path:      r.URL.Path,
				Protocol:  r.Proto,
				SourceIP:  requestSourceIP(r),
				UserAgent: r.UserAgent(),
			},
		},
		Body:            requestBody,
		IsBase64Encoded: encoded,
	}
}

func apiGatewayV2Headers(r *http.Request) map[string]string {
	headers := make(map[string]string, len(r.Header)+1)
	for key, values := range r.Header {
		key = strings.ToLower(key)
		if key == "cookie" {
			continue
		}
		headers[key] = strings.Join(values, ",")
	}
	if _, ok := headers["host"]; !ok {
		headers["host"] = r.Host
	}
	headers["x-forwarded-proto"] = requestScheme(r)
	headers["x-forwarded-for"] = requestSourceIP(r)
	headers["x-forwarded-port"] = requestPort(r)
	return headers
}

func apiGatewayV2Cookies(r *http.Request) []string {
	var values []string
	for _, header := range r.Header.Values("Cookie") {
		if header != "" {
			values = append(values, strings.Split(header, "; ")...)
		}
	}
	return values
}

func apiGatewayV2QueryParameters(r *http.Request) map[string]string {
	query := r.URL.Query()
	parameters := make(map[string]string, len(query))
	for key, values := range query {
		parameters[key] = strings.Join(values, ",")
	}
	return parameters
}

func requestSourceIP(r *http.Request) string {
	if clientIP, ok := caddyhttp.GetVar(r.Context(), caddyhttp.ClientIPVarKey).(string); ok && clientIP != "" {
		return clientIP
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func requestScheme(r *http.Request) string {
	if r.URL.Scheme != "" {
		return r.URL.Scheme
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func requestPort(r *http.Request) string {
	if localAddr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if _, port, err := net.SplitHostPort(localAddr.String()); err == nil && port != "" {
			return port
		}
	}
	if requestScheme(r) == "https" {
		return "443"
	}
	return "80"
}

func requestDomainName(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.Host); err == nil {
		return host
	}
	return r.Host
}

func requestBodyIsBase64Encoded(r *http.Request, body []byte) bool {
	if len(body) == 0 {
		return false
	}
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])
	}
	if isTextualMediaType(mediaType) {
		return false
	}
	return !utf8.Valid(body) || contentType != ""
}

func isTextualMediaType(mediaType string) bool {
	return strings.HasPrefix(mediaType, "text/") ||
		mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") ||
		mediaType == "application/javascript" ||
		mediaType == "application/xml" || strings.HasSuffix(mediaType, "+xml") ||
		mediaType == "application/x-www-form-urlencoded" ||
		mediaType == "application/x-ndjson" ||
		mediaType == "application/graphql"
}

// APIGatewayV2Request is the HTTP API payload format consumed by Mangum.
type APIGatewayV2Request struct {
	Version               string                     `json:"version"`
	RouteKey              string                     `json:"routeKey"`
	RawPath               string                     `json:"rawPath"`
	RawQueryString        string                     `json:"rawQueryString"`
	Headers               map[string]string          `json:"headers"`
	Cookies               []string                   `json:"cookies,omitempty"`
	QueryStringParameters map[string]string          `json:"queryStringParameters,omitempty"`
	RequestContext        APIGatewayV2RequestContext `json:"requestContext"`
	Body                  string                     `json:"body,omitempty"`
	IsBase64Encoded       bool                       `json:"isBase64Encoded"`
}

// APIGatewayV2Response is the response format emitted by Mangum.
type APIGatewayV2Response struct {
	StatusCode      int               `json:"statusCode"`
	Headers         map[string]string `json:"headers"`
	Cookies         []string          `json:"cookies"`
	Body            string            `json:"body"`
	IsBase64Encoded bool              `json:"isBase64Encoded"`
}

type APIGatewayV2RequestContext struct {
	RequestID  string                  `json:"requestId"`
	TimeEpoch  int64                   `json:"timeEpoch"`
	DomainName string                  `json:"domainName"`
	Stage      string                  `json:"stage"`
	HTTP       APIGatewayV2HTTPContext `json:"http"`
}

type APIGatewayV2HTTPContext struct {
	Method    string `json:"method"`
	Path      string `json:"path"`
	Protocol  string `json:"protocol"`
	SourceIP  string `json:"sourceIp"`
	UserAgent string `json:"userAgent"`
}

// newRequestMeta returns a new RequestMeta based on the HTTP request
func newRequestMeta(r *http.Request) *RequestMeta {
	headers := make(map[string][]string)
	for k, v := range r.Header {
		headers[strings.ToLower(k)] = v
	}
	return &RequestMeta{
		Method:  r.Method,
		Path:    r.URL.Path,
		Query:   r.URL.RawQuery,
		Host:    r.Host,
		Proto:   r.Proto,
		Headers: headers,
	}
}

// Request represents a single HTTP request.  It will be serialized as JSON
// and sent to the AWS Lambda function as the function payload.
type Request struct {
	// Set to the constant "HTTPJSON-REQ"
	Type string `json:"type"`
	// Metadata about the HTTP request
	Meta *RequestMeta `json:"meta"`
	// HTTP request body (may be empty)
	Body string `json:"body"`
	// Encoding of Body - Valid values: "", "base64"
	BodyEncoding string `json:"bodyEncoding"`
}

// RequestMeta represents HTTP metadata present on the request
type RequestMeta struct {
	// HTTP method used by client (e.g. GET or POST)
	Method string `json:"method"`

	// Path portion of URL without the query string
	Path string `json:"path"`

	// Query string (without '?')
	Query string `json:"query"`

	// Host field from net/http Request, which may be of the form host:port
	Host string `json:"host"`

	// Proto field from net/http Request, for example "HTTP/1.1"
	Proto string `json:"proto"`

	// HTTP request headers
	Headers map[string][]string `json:"headers"`
}

// Reply encapsulates the response from a Lambda invocation.
// AWS Lambda functions should return a JSON object that matches this format.
type Reply struct {
	// Must be set to the constant "HTTPJSON-REP"
	Type string `json:"type"`
	// Reply metadata. If omitted, a default 200 status with empty headers will be used.
	Meta *ReplyMeta `json:"meta"`
	// Response body
	Body string `json:"body"`
	// Encoding of Body - Valid values: "", "base64"
	BodyEncoding string `json:"bodyEncoding"`
}

// ReplyMeta encapsulates HTTP response metadata that the lambda function wishes
// Caddy to set on the HTTP response.
//
// *NOTE* that header values must be encoded as string arrays
type ReplyMeta struct {
	// HTTP status code (e.g. 200 or 404)
	Status int `json:"status"`
	// HTTP response headers
	Headers map[string][]string `json:"headers"`
}
