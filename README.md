# caddy-awslambda

Caddy v2 module for dispatching requests to AWS Lambda.

This is a port of https://github.com/coopernurse/caddy-awslambda with less features but for Caddy 2.

## Installation

```
xcaddy build \
    --with github.com/floj/caddy-awslambda
```

## Usage

### Minimal configuration

The only required setting is the Lambda function name. The AWS SDK uses its
default credential and region resolution, the `httpjson` event format, and the
default timeout and body-size limit.

```caddyfile
:8080 {
  handle /services/* {
    awslambda {
      function ForwardToSlack
    }
  }
}
```

### Full configuration

```caddyfile
:8080 {
  log {
    output stderr
  }
  handle /services/* {
    awslambda {
      function ForwardToSlack
      qualifier prod
      event_format api_gateway_v2
      timeout 10s
      max_body_size 1048576
      header_upstream X-Forwarded-Host {http.request.host}
      region us-east-1

      # Key based auth
      access_key_id local-access-key
      secret_access_key local-secret-key
      session_token local-session-token

      # OR Role based auth
      # role_arn arn:aws:iam::123456789012:role/LambdaInvoker
      # external_id example-external-id
      # session_name caddy-awslambda

      # Useful for local testing:
      # endpoint http://127.0.0.1:3001
    }
  }
}
```

| Setting             | Description                                      | Default                  |
| ------------------- | ------------------------------------------------ | ------------------------ |
| `function`          | Lambda function name or ARN to invoke.           | (required)               |
| `qualifier`         | Lambda version number or alias to invoke.        | Unqualified function     |
| `event_format`      | Lambda request and response contract.            | `httpjson`               |
| `timeout`           | Maximum duration of a synchronous invocation.    | `10s`                    |
| `max_body_size`     | Maximum request body size in bytes.              | `0` (unlimited)          |
| `header_upstream`   | Header values added to the Lambda request.       | Not set                  |
| `region`            | AWS region used for the Lambda client.           | AWS SDK resolution       |
| `access_key_id`     | Optional static access key for local testing.    | AWS SDK credential chain |
| `secret_access_key` | Static secret key paired with `access_key_id`.   | AWS SDK credential chain |
| `session_token`     | Optional token for static temporary credentials. | Not set                  |
| `role_arn`          | IAM role to assume before invoking Lambda.       | No role assumption       |
| `external_id`       | External ID passed to STS `AssumeRole`.          | Not set                  |
| `session_name`      | Role session name passed to STS `AssumeRole`.    | AWS SDK default          |
| `endpoint`          | Lambda API endpoint.                             | AWS SDK endpoint         |

When the credential settings are omitted, the AWS SDK default credential chain
is used. `role_arn`, `external_id`, and `session_name` configure optional STS
role assumption as described below.

Lambda invocation uses Caddy's IAM identity; end-user authentication and
authorization remain the application's responsibility, with request headers
such as `Authorization` forwarded unchanged.

## IAM permissions

Caddy invokes Lambda with SigV4 using its workload IAM role. Grant that role
only permission to invoke the target function or alias:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": "lambda:InvokeFunction",
      "Resource": "arn:aws:lambda:us-east-1:123456789012:function:ForwardToSlack"
    }
  ]
}
```

Use the qualified function or alias ARN as `Resource` when invocation is
restricted to a specific version or alias. If `role_arn` is configured, the
workload role also needs `sts:AssumeRole` permission for that target role.

For cross-account access, choose one of these patterns:

- Direct invocation: add the Caddy workload role as a principal in the Lambda
  function's resource-based policy, and grant the workload role
  `lambda:InvokeFunction` on that function.
- Assumed role: grant the workload role `sts:AssumeRole` on the target role,
  trust the workload role in the target role's trust policy, and grant the
  target role `lambda:InvokeFunction` on the function. In this pattern, the
  Lambda resource policy is not required because the assumed role belongs to
  the target account.

## Event formats

The `event_format` setting selects both the request event and response format.

| Value            | Status    | Contract                                             |
| ---              | ---       | ---                                                  |
| `httpjson`       | Default   | `HTTPJSON-REQ` and `HTTPJSON-REP` envelopes.         |
| `api_gateway_v2` | Available | API Gateway HTTP API payload version 2.0 for Mangum. |
| `api_gateway_v1` | Planned   | API Gateway REST/proxy payload version 1.0.          |
| `alb`            | Planned   | Application Load Balancer Lambda event.              |
| `function_url`   | Planned   | Lambda Function URL event.                           |
| `lambda_at_edge` | Planned   | CloudFront Lambda@Edge event.                        |

### API Gateway v2

For API Gateway v2, request headers and duplicate query parameters use the
service's comma-joined representation, cookies use the `cookies` list, and the
raw path and query string are preserved in `rawPath` and `rawQueryString`.
Empty bodies are sent with base64 encoding disabled. Binary bodies use base64
encoding and set `isBase64Encoded` to `true`. Responses use `statusCode`,
`headers`, `cookies`, `body`, and `isBase64Encoded`; each response cookie is
emitted as a separate `Set-Cookie` header.

The initial API Gateway v2 adapter maps Caddy's request path directly to
`rawPath`; it does not trim a base path. AWS invocation errors, Lambda function
errors, timeouts, throttling, and malformed responses are returned as Caddy
handler errors and are not silently converted to successful responses.

### Logging

HTTP access logging remains Caddy's responsibility. This module does not log
credentials, headers, or request/response bodies.

At `DEBUG` level, every attempted Lambda invocation emits the function name,
invocation duration, and an incoming `X-Request-ID` when present. Failed
invocations also include the contextual error. At `INFO`, `WARN`, and `ERROR`
levels, the module does not emit a separate per-invocation record. Invocation
failures are returned as contextual handler errors for Caddy to log and convert
into an HTTP error response through its normal error handling configuration.

## Development

Use mise commands to lint, format, and test the code.

```
mise lint
mise format
mise test
mise e2e
```

## License

Apache 2
