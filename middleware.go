package caddylambda

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
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

func init() {
	caddy.RegisterModule(&Lambda{})
}

// Lambda implements an HTTP handler that invokes a Lambda function.
type Lambda struct {
	FunctionName    string              `json:"function,omitempty"`
	Qualifier       string              `json:"qualifier,omitempty"`
	Endpoint        string              `json:"endpoint,omitempty"`
	Region          string              `json:"region,omitempty"`
	AccessKeyID     string              `json:"access_key_id,omitempty"`
	SecretAccessKey string              `json:"secret_access_key,omitempty"`
	SessionToken    string              `json:"session_token,omitempty"`
	EventFormat     string              `json:"event_format,omitempty"`
	Timeout         string              `json:"timeout,omitempty"`
	MaxBodySize     int64               `json:"max_body_size,omitempty"`
	RoleARN         string              `json:"role_arn,omitempty"`
	ExternalID      string              `json:"external_id,omitempty"`
	SessionName     string              `json:"session_name,omitempty"`
	UpstreamHeaders map[string][]string `json:"header_upstream,omitempty"`

	timeout time.Duration
	log     *zap.Logger
	svc     lambdaInvoker
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

	if m.Timeout == "" {
		m.Timeout = "10s"
	}

	dur, err := time.ParseDuration(m.Timeout)
	if err != nil {
		return fmt.Errorf("invalid value for timeout: %w", err)
	}
	m.timeout = dur

	configOptions := []func(*config.LoadOptions) error{}
	if m.Region != "" {
		configOptions = append(configOptions, config.WithRegion(m.Region))
	}
	if m.AccessKeyID != "" {
		configOptions = append(configOptions, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			m.AccessKeyID, m.SecretAccessKey, m.SessionToken,
		)))
	}
	cfg, err := config.LoadDefaultConfig(ctx, configOptions...)
	if err != nil {
		return fmt.Errorf("unable to load AWS config: %w", err)
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

// Validate implements caddy.Validator.
func (m *Lambda) Validate() error {
	if m.Timeout != "" {
		dur, err := time.ParseDuration(m.Timeout)
		if err != nil {
			return fmt.Errorf("invalid value for timeout: %w", err)
		}
		if dur <= 0 {
			return errors.New("timeout must be greater than zero")
		}
	}
	if (m.AccessKeyID == "") != (m.SecretAccessKey == "") {
		return errors.New("access_key_id and secret_access_key must be configured together")
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
	req, err := newRequestForFormat(r, m.eventFormat(), m.MaxBodySize, m.UpstreamHeaders)
	if err != nil {
		return err
	}

	resp, err := m.invokeLambda(r.Context(), req, r.Header.Get("X-Request-ID"))

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
	_, err = w.Write(bodyBytes)
	if err != nil || reply.Meta.Status >= 400 {
		return err
	}

	return nil
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
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
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

// Cleanup implements caddy.Cleanup.
// TODO: ensure all running processes are terminated.
func (m *Lambda) Cleanup() error {
	return nil
}

func defaultReplyMeta() *ReplyMeta {
	return &ReplyMeta{
		Status:  http.StatusOK,
		Headers: map[string][]string{"content-type": {"application/json"}},
	}
}
