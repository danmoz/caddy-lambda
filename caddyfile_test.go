package caddylambda

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
)

func TestUnmarshalCaddyfileEndpoint(t *testing.T) {
	m := &Lambda{}
	if err := m.UnmarshalCaddyfile(caddyfile.NewTestDispenser(`
		lambda {
			function test-function
			qualifier prod
			endpoint http://127.0.0.1:3001
			region us-east-1
			timeout 1d
			max_body_size 1MB
			role_arn arn:aws:iam::123456789012:role/test
			external_id external-test
			session_name caddy-test
		}
	`)); err != nil {
		t.Fatalf("UnmarshalCaddyfile() error = %v", err)
	}

	if m.FunctionName != "test-function" {
		t.Errorf("FunctionName = %q, want %q", m.FunctionName, "test-function")
	}
	if m.Qualifier != "prod" {
		t.Errorf("Qualifier = %q, want %q", m.Qualifier, "prod")
	}
	if m.Endpoint != "http://127.0.0.1:3001" {
		t.Errorf("Endpoint = %q, want %q", m.Endpoint, "http://127.0.0.1:3001")
	}
	if m.Region != "us-east-1" {
		t.Errorf("Region = %q, want %q", m.Region, "us-east-1")
	}
	if time.Duration(m.Timeout) != 24*time.Hour {
		t.Errorf("Timeout = %v, want %v", m.Timeout, 24*time.Hour)
	}
	if m.MaxBodySize != 1000000 {
		t.Errorf("MaxBodySize = %d, want 1000000", m.MaxBodySize)
	}
	if m.RoleARN != "arn:aws:iam::123456789012:role/test" || m.ExternalID != "external-test" || m.SessionName != "caddy-test" {
		t.Errorf("role settings = %q/%q/%q", m.RoleARN, m.ExternalID, m.SessionName)
	}
}

func TestUnmarshalCaddyfileAllowsMatcher(t *testing.T) {
	m := &Lambda{}
	err := m.UnmarshalCaddyfile(caddyfile.NewTestDispenser(`
		lambda /services/* {
			function test-function
		}
	`))
	if err != nil {
		t.Fatalf("UnmarshalCaddyfile() error = %v", err)
	}
	if m.FunctionName != "test-function" {
		t.Errorf("FunctionName = %q, want %q", m.FunctionName, "test-function")
	}
}

func TestUnmarshalCaddyfileRejectsDuplicateEventFormat(t *testing.T) {
	m := &Lambda{}
	err := m.UnmarshalCaddyfile(caddyfile.NewTestDispenser(`
		lambda {
			event_format httpjson
			event_format api_gateway_v2
		}
	`))
	if err == nil {
		t.Fatal("UnmarshalCaddyfile() error = nil, want duplicate directive error")
	}
}

func TestUnmarshalCaddyfileRejectsDuplicateQualifier(t *testing.T) {
	m := &Lambda{}
	err := m.UnmarshalCaddyfile(caddyfile.NewTestDispenser(`
		lambda {
			qualifier prod
			qualifier staging
		}
	`))
	if err == nil {
		t.Fatal("UnmarshalCaddyfile() error = nil, want duplicate qualifier error")
	}
}

func TestUnmarshalCaddyfileParsesUpstreamHeaders(t *testing.T) {
	m := &Lambda{}
	err := m.UnmarshalCaddyfile(caddyfile.NewTestDispenser(`
		lambda {
			header_upstream X-API-Key secret
			header_upstream X-API-Key backup
			header_upstream X-Forwarded-Host {http.request.host}
		}
	`))
	if err != nil {
		t.Fatalf("UnmarshalCaddyfile() error = %v", err)
	}
	if got := m.UpstreamHeaders["X-API-Key"]; len(got) != 2 || got[0] != "secret" || got[1] != "backup" {
		t.Errorf("X-API-Key = %#v, want [secret backup]", got)
	}
	if got := m.UpstreamHeaders["X-Forwarded-Host"]; len(got) != 1 || got[0] != "{http.request.host}" {
		t.Errorf("X-Forwarded-Host = %#v, want placeholder", got)
	}
}

func TestUnmarshalCaddyfileRejectsExtraUpstreamHeaderArgs(t *testing.T) {
	m := &Lambda{}
	err := m.UnmarshalCaddyfile(caddyfile.NewTestDispenser(`
		lambda {
			header_upstream X-API-Key secret extra
		}
	`))
	if err == nil {
		t.Fatal("UnmarshalCaddyfile() error = nil, want argument error")
	}
}

func TestCaddyfileAdaptsLambdaHandler(t *testing.T) {
	adapter := caddyfile.Adapter{ServerType: httpcaddyfile.ServerType{}}
	result, _, err := adapter.Adapt([]byte(`:8080 {
	route {
		lambda {
			function test-function
			region us-east-1
			timeout 1d
			max_body_size 1MB
			header_upstream X-Test value
		}
	}
}`), nil)
	if err != nil {
		t.Fatalf("adapt Caddyfile: %v", err)
	}

	var config map[string]any
	if err := json.Unmarshal(result, &config); err != nil {
		t.Fatalf("decode adapted JSON: %v", err)
	}

	apps := config["apps"].(map[string]any)
	httpApp := apps["http"].(map[string]any)
	servers := httpApp["servers"].(map[string]any)
	server := servers["srv0"].(map[string]any)
	routes := server["routes"].([]any)
	subroute := routes[0].(map[string]any)["handle"].([]any)[0].(map[string]any)
	nestedRoute := subroute["routes"].([]any)[0].(map[string]any)
	lambda := nestedRoute["handle"].([]any)[0].(map[string]any)

	if lambda["handler"] != "lambda" || lambda["function"] != "test-function" {
		t.Fatalf("adapted handler = %#v", lambda)
	}
	if lambda["region"] != "us-east-1" || lambda["timeout"] != float64(24*time.Hour) || lambda["max_body_size"] != float64(1000000) {
		t.Fatalf("adapted settings = %#v", lambda)
	}
	if got := lambda["header_upstream"].(map[string]any)["X-Test"].([]any)[0]; got != "value" {
		t.Fatalf("header_upstream X-Test = %#v, want value", got)
	}
}
