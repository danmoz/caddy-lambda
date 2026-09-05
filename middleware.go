package caddylambda

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

type lambdaInvoker interface {
	Invoke(context.Context, *lambda.InvokeInput, ...func(*lambda.Options)) (*lambda.InvokeOutput, error)
}

var (
	_ caddy.Module                = (*Lambda)(nil)
	_ caddy.Provisioner           = (*Lambda)(nil)
	_ caddy.Validator             = (*Lambda)(nil)
	_ caddyhttp.MiddlewareHandler = (*Lambda)(nil)
)

const maxLoggedRequestIDLength = 256

// Shared across all Lambda handler instances so every route reuses a single
// AWS config and HTTP client (and thus a single connection pool) instead of
// spinning up one per route.
var (
	awsConfigOnce sync.Once
	awsConfig     aws.Config
	awsConfigErr  error
)

func loadAWSConfig(ctx context.Context) (aws.Config, error) {
	awsConfigOnce.Do(func() {
		httpClient := awshttp.NewBuildableClient().WithTransportOptions(func(transport *http.Transport) {
			transport.MaxIdleConnsPerHost = 100
		})
		awsConfig, awsConfigErr = config.LoadDefaultConfig(
			ctx,
			config.WithRetryMaxAttempts(1),
			config.WithHTTPClient(httpClient),
		)
	})
	return awsConfig, awsConfigErr
}

func init() {
	caddy.RegisterModule(&Lambda{})
}

// Lambda implements an HTTP handler that invokes a Lambda function.
type Lambda struct {
	FunctionName    string              `json:"function,omitempty"`
	Qualifier       string              `json:"qualifier,omitempty"`
	Endpoint        string              `json:"endpoint,omitempty"`
	Region          string              `json:"region,omitempty"`
	EventFormat     string              `json:"event_format,omitempty"`
	Timeout         caddy.Duration      `json:"timeout,omitempty"`
	MaxBodySize     int64               `json:"max_body_size,omitempty"`
	RoleARN         string              `json:"role_arn,omitempty"`
	ExternalID      string              `json:"external_id,omitempty"`
	SessionName     string              `json:"session_name,omitempty"`
	UpstreamHeaders map[string][]string `json:"header_upstream,omitempty"`

	log *zap.Logger
	svc lambdaInvoker
}

// CaddyModule returns the Caddy module information.
func (*Lambda) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.lambda",
		New: func() caddy.Module { return &Lambda{} },
	}
}

// Provision implements caddy.Provisioner.
func (m *Lambda) Provision(ctx caddy.Context) error {
	m.log = ctx.Logger(m)
	if isInsecureEndpoint(m.Endpoint) {
		m.log.Warn("custom Lambda endpoint uses HTTP; AWS credentials may be transmitted in plaintext")
	}

	if m.Timeout == 0 {
		m.Timeout = caddy.Duration(10 * time.Second)
	}

	cfg, err := loadAWSConfig(ctx)
	if err != nil {
		return fmt.Errorf("unable to load AWS config: %w", err)
	}
	if m.Region != "" {
		cfg.Region = m.Region
	}
	if cfg.Region == "" {
		return errors.New("AWS region is required")
	}
	if m.RoleARN != "" {
		provider := stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), m.RoleARN, func(options *stscreds.AssumeRoleOptions) {
			if m.ExternalID != "" {
				options.ExternalID = aws.String(m.ExternalID)
			}
			if m.SessionName != "" {
				options.RoleSessionName = m.SessionName
			}
		})
		cfg.Credentials = aws.NewCredentialsCache(provider)
	}

	if m.Endpoint == "" {
		m.svc = lambda.NewFromConfig(cfg)
	} else {
		m.svc = lambda.NewFromConfig(cfg, func(options *lambda.Options) {
			options.BaseEndpoint = &m.Endpoint
		})
	}
	return nil
}

func isInsecureEndpoint(endpoint string) bool {
	parsed, err := url.Parse(endpoint)
	return err == nil && strings.EqualFold(parsed.Scheme, "http")
}

// Validate implements caddy.Validator.
func (m *Lambda) Validate() error {
	if m.Timeout < 0 {
		return errors.New("timeout must be greater than zero")
	}
	if m.RoleARN == "" && (m.ExternalID != "" || m.SessionName != "") {
		return errors.New("external_id and session_name require role_arn")
	}
	if m.MaxBodySize < 0 {
		return errors.New("max_body_size must not be negative")
	}
	if m.FunctionName == "" {
		return errors.New("function must be configured")
	}

	switch m.eventFormat() {
	case eventFormatHTTPJSON, eventFormatAPIGatewayV2:
		return nil
	default:
		return fmt.Errorf("unsupported event format %q", m.EventFormat)
	}
}

// ServeHTTP implements caddyhttp.MiddlewareHandler.
func (m *Lambda) ServeHTTP(w http.ResponseWriter, r *http.Request, _ caddyhttp.Handler) error {
	req, err := newRequestForFormat(w, r, m.eventFormat(), m.MaxBodySize, m.UpstreamHeaders)
	if err != nil {
		return err
	}

	requestID := r.Header.Get("X-Request-ID")
	if len(requestID) > maxLoggedRequestIDLength {
		requestID = requestID[:maxLoggedRequestIDLength]
	}
	resp, err := m.invokeLambda(r.Context(), req, requestID)

	if err != nil {
		return caddyhttp.Error(lambdaErrorStatus(err), err)
	}

	// Unpack the reply JSON
	reply, err := parseReply(resp, m.eventFormat())
	if err != nil {
		return err
	}
	if err := validateReply(reply); err != nil {
		return err
	}

	// Optionally decode the response body before writing any response data.
	var bodyBytes []byte
	if reply.BodyEncoding == "base64" && reply.Body != "" {
		bodyBytes, err = base64.StdEncoding.DecodeString(reply.Body)
		if err != nil {
			return err
		}
	} else {
		bodyBytes = []byte(reply.Body)
	}

	// Write the response HTTP headers
	for k, vals := range reply.Meta.Headers {
		if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") || strings.EqualFold(k, "Connection") {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}

	// Default the Content-Type to application/json if not provided on reply
	if w.Header().Get("content-type") == "" {
		w.Header().Set("content-type", "application/json")
	}
	if reply.Meta.Status <= 0 {
		reply.Meta.Status = http.StatusOK
	}

	w.WriteHeader(reply.Meta.Status)

	// Write the response body
	if reply.Meta.Status == http.StatusNoContent || reply.Meta.Status == http.StatusNotModified {
		return nil
	}
	_, err = w.Write(bodyBytes)
	return err
}

func lambdaErrorStatus(err error) int {
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "TooManyRequestsException" {
		return http.StatusServiceUnavailable
	}

	return http.StatusBadGateway
}

func (m *Lambda) invokeLambda(ctx context.Context, req any, requestID string) (payload []byte, err error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(m.Timeout))
	defer cancel()

	fields := []zap.Field{zap.String("function", m.FunctionName)}
	if requestID != "" {
		fields = append(fields, zap.String("request_id", requestID))
	}
	log := m.log.With(fields...)
	startTime := time.Now()
	defer func() {
		logFields := []zap.Field{zap.Duration("duration", time.Since(startTime))}
		if err != nil {
			logFields = append(logFields, zap.Error(err))
		}
		log.Debug("Lambda invocation complete", logFields...)
	}()

	payload, err = json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal Lambda request for %q: %w", m.FunctionName, err)
	}

	input := &lambda.InvokeInput{
		FunctionName: &m.FunctionName,
		Payload:      payload,
	}
	if m.Qualifier != "" {
		input.Qualifier = aws.String(m.Qualifier)
	}

	resp, err := m.svc.Invoke(ctx, input)

	if err != nil {
		return nil, fmt.Errorf("invoke Lambda %q: %w", m.FunctionName, err)
	}

	if resp.FunctionError != nil {
		return nil, fmt.Errorf("Lambda function %q returned %s", m.FunctionName, *resp.FunctionError)
	}

	return resp.Payload, nil
}

func (m *Lambda) eventFormat() string {
	if m.EventFormat == "" {
		return eventFormatAPIGatewayV2
	}
	return m.EventFormat
}
