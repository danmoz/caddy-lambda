package caddylambda

import (
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

func TestUnmarshalCaddyfileEndpoint(t *testing.T) {
	m := &Lambda{}
	if err := m.UnmarshalCaddyfile(caddyfile.NewTestDispenser(`
		lambda {
			function test-function
			qualifier prod
			endpoint http://127.0.0.1:3001
			region us-east-1
			timeout 5s
			max_body_size 1024
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
	if m.Timeout != "5s" {
		t.Errorf("Timeout = %q, want %q", m.Timeout, "5s")
	}
	if m.MaxBodySize != 1024 {
		t.Errorf("MaxBodySize = %d, want 1024", m.MaxBodySize)
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
			header_upstream X-Forwarded-Host {http.request.host}
		}
	`))
	if err != nil {
		t.Fatalf("UnmarshalCaddyfile() error = %v", err)
	}
	if got := m.UpstreamHeaders["X-API-Key"]; len(got) != 1 || got[0] != "secret" {
		t.Errorf("X-API-Key = %#v, want [secret]", got)
	}
	if got := m.UpstreamHeaders["X-Forwarded-Host"]; len(got) != 1 || got[0] != "{http.request.host}" {
		t.Errorf("X-Forwarded-Host = %#v, want placeholder", got)
	}
}
