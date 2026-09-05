package caddylambda

import (
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/dustin/go-humanize"
)

func init() {
	httpcaddyfile.RegisterHandlerDirective("lambda", parseCaddyfile)
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	m := &Lambda{}
	err := m.UnmarshalCaddyfile(h.Dispenser)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// UnmarshalCaddyfile configures the global directive from Caddyfile.
// Syntax:
//
//	lambda [<matcher>] {
//	    function <function name>
//	    qualifier <version or alias>
//	    endpoint <url>
//	    region <region>
//	    timeout  <duration>
//	    max_body_size <bytes>
//	    role_arn <role ARN>
//	    external_id <external ID>
//	    session_name <session name>
//	    header_upstream <header> <value>
//	}
func (m *Lambda) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		for d.NextBlock(0) {
			switch d.Val() {
			case "function":
				if m.FunctionName != "" {
					return d.Err("function already specified")
				}
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.FunctionName = d.Val()
			case "qualifier":
				if m.Qualifier != "" {
					return d.Err("qualifier already specified")
				}
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.Qualifier = d.Val()
			case "endpoint":
				if m.Endpoint != "" {
					return d.Err("endpoint already specified")
				}
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.Endpoint = d.Val()
			case "region":
				if m.Region != "" {
					return d.Err("region already specified")
				}
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.Region = d.Val()
			case "timeout":
				if m.Timeout != 0 {
					return d.Err("timeout already specified")
				}
				if !d.NextArg() {
					return d.ArgErr()
				}
				duration, err := caddy.ParseDuration(d.Val())
				if err != nil {
					return d.Errf("invalid timeout: %v", err)
				}
				m.Timeout = caddy.Duration(duration)
			case "event_format":
				if m.EventFormat != "" {
					return d.Err("event_format already specified")
				}
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.EventFormat = d.Val()
			case "max_body_size":
				if m.MaxBodySize != 0 {
					return d.Err("max_body_size already specified")
				}
				if !d.NextArg() {
					return d.ArgErr()
				}
				size, err := humanize.ParseBytes(d.Val())
				if err != nil {
					return d.Errf("invalid max_body_size: %v", err)
				}
				if size > uint64(^uint64(0)>>1) {
					return d.Err("max_body_size is too large")
				}
				m.MaxBodySize = int64(size)
			case "role_arn":
				if m.RoleARN != "" {
					return d.Err("role_arn already specified")
				}
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.RoleARN = d.Val()
			case "external_id":
				if m.ExternalID != "" {
					return d.Err("external_id already specified")
				}
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.ExternalID = d.Val()
			case "session_name":
				if m.SessionName != "" {
					return d.Err("session_name already specified")
				}
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.SessionName = d.Val()
			case "header_upstream":
				args := d.RemainingArgs()
				if len(args) != 2 {
					return d.ArgErr()
				}
				if m.UpstreamHeaders == nil {
					m.UpstreamHeaders = make(map[string][]string)
				}
				m.UpstreamHeaders[args[0]] = append(m.UpstreamHeaders[args[0]], args[1])
			default:
				return d.Errf("unrecognized subdirective: %s", d.Val())
			}
		}
	}
	return nil
}
