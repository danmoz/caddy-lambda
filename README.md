# caddy-lambda

[Caddy](https://caddyserver.com/) v2 plugin for dispatching requests to [AWS Lambda](https://aws.amazon.com/lambda/).

## Installation

Use the standard [xcaddy](https://github.com/caddyserver/xcaddy) tool to build a custom `caddy` binary that includes the plugin.
```
xcaddy build --with github.com/danmoz/caddy-lambda
```

## Usage

### Minimal configuration

The Lambda function name is required, and an AWS region must be available either
through the `region` setting or the AWS SDK's environment/shared configuration.
By default, the plugin will forward events using the `api_gateway_v2` event format.

```caddyfile
:8080 {
  handle /services/* {
    lambda {
      function MyLambdaFunction
      region us-east-1
    }
  }
}
```

### Full configuration

```caddyfile
:8080 {
  handle /services/* {
    lambda {
      function MyLambdaFunction
      qualifier prod
      region us-east-1
      event_format api_gateway_v2
      timeout 10s
      max_body_size 1MB
      header_upstream X-Forwarded-Host {http.request.host}

      # Recommended for production: assume a dedicated IAM role
      role_arn arn:aws:iam::123456789012:role/LambdaInvoker
      external_id example-external-id
      session_name caddy-lambda

      # Useful for local testing:
      # endpoint http://127.0.0.1:3001
    }
  }
}
```

| Setting             | Description                                      | Default                  |
| ------------------- | ------------------------------------------------ | ------------------------ |
| `function`          | Lambda function name or ARN to invoke.           | (required)               |
| `region`            | AWS region used for the Lambda client.           | (required if no SDK)     |
| `qualifier`         | Lambda version number or alias to invoke.        | Unqualified function     |
| `event_format`      | Lambda request and response contract.            | `api_gateway_v2`         |
| `timeout`           | Max duration of a synchronous invocation.        | `10s`                    |
| `max_body_size`     | Max request body size; Caddyfile accepts units.  | `4MB`                    |
| `header_upstream`   | Header values added to the Lambda request.       | Not set                  |
| `role_arn`          | IAM role to assume before invoking Lambda.       | No role assumption       |
| `external_id`       | External ID passed to STS `AssumeRole`.          | Not set                  |
| `session_name`      | Role session name passed to STS `AssumeRole`.    | AWS SDK default          |
| `endpoint`          | Lambda API endpoint.                             | AWS SDK endpoint         |

## Configuration Tips

For production, prefer an IAM role attached to the Caddy runtime (EC2, ECS, etc.). 
Runtime roles provide short-lived, automatically rotated credentials without 
requiring long-lived secrets to be stored or managed.

If Caddy requires a separate permission set or needs to invoke Lambda functions 
in another AWS account, use `role_arn` to assume a dedicated role with only the 
required Lambda permissions.

For local development, the standard AWS SDK credential chain also supports 
environment variables and shared AWS configuration files. Avoid using these 
methods in production where a runtime role is available.

Lambda invocation uses Caddy's AWS identity. End-user authentication and 
authorization remain the application's responsibility; request headers such as 
`Authorization` are forwarded unchanged.

Lambda invocations are not automatically retried. If an invocation fails after 
AWS has accepted the request, Caddy will be unable to determine whether the Lambda 
function executed successfully. Retrying in this situation could result in 
duplicate invocations.

Be careful when increasing `max_body_size`. AWS Lambda limits synchronous invocation payloads to 6 MB, and this limit applies to the complete serialized event sent to Lambda, not just the HTTP request body. Binary request bodies are base64-encoded, increasing their size by approximately 33%, with headers and the event envelope adding further overhead.

The default 4 MiB limit leaves some headroom, but unusually large headers may reduce the maximum safe body size. If the serialized Lambda event exceeds the 6 MB limit, caddy-lambda rejects the request with HTTP `413 Payload Too Large`.

## Response Codes

| Status | Meaning |
| --- | --- |
| `413` | Request body exceeds `max_body_size` (4 MiB by default). |
| `500` | Malformed Lambda responses, invalid response bodies, and other adapter errors. |
| `502` | Lambda invocation failed for a reason other than timeout or throttling, including a Lambda function error. |
| `503` | AWS rejected the invocation with `TooManyRequestsException`. |
| `504` | Lambda invocation exceeded the configured timeout. |

The plugin does not generate application statuses such as `400`, `401`, `403`,
or `404`; Lambda responses should supply those statuses when appropriate.

## IAM permissions

Caddy invokes Lambda with SigV4 using credentials resolved from the AWS SDK
credential chain or an assumed role. Grant the IAM principal represented by
those credentials permission to invoke the target function or alias:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": "lambda:InvokeFunction",
      "Resource": "arn:aws:lambda:us-east-1:123456789012:function:MyLambdaFunction"
    }
  ]
}
```

Use the qualified function or alias ARN as `Resource` when invocation is
restricted to a specific version or alias. If `role_arn` is configured, the
source credentials also need `sts:AssumeRole` permission for that target role,
and the target role needs the `lambda:InvokeFunction` permission above.

For cross-account access, choose one of these patterns:

- Direct invocation: add the IAM principal represented by the configured
  credentials as a principal in the Lambda function's resource-based policy,
  and grant that principal `lambda:InvokeFunction` on the function.
- Assumed role: grant the source credentials `sts:AssumeRole` on the target
  role, trust the source principal in the target role's trust policy, and grant
  the target role `lambda:InvokeFunction` on the function. In this pattern, the
  Lambda resource policy is not required because the assumed role belongs to
  the target account.

## Event formats

The `event_format` setting selects both the request event and response format.

| Value            | Status    | Contract                                             |
| ---              | ---       | ---                                                  |
| `httpjson`       | Available | `HTTPJSON-REQ` and `HTTPJSON-REP` envelopes.         |
| `api_gateway_v2` | Default   | API Gateway HTTP API payload version 2.0.            |
| `api_gateway_v1` | Planned   | API Gateway REST/proxy payload version 1.0.          |
| `alb`            | Planned   | Application Load Balancer Lambda event.              |
| `function_url`   | Planned   | Lambda Function URL event.                           |
| `lambda_at_edge` | Planned   | CloudFront Lambda@Edge event.                        |

### HTTPJSON

The `httpjson` format sends a JSON object with a `type` of
`HTTPJSON-REQ`, request metadata in `meta`, and the request body in `body`.
Binary request bodies use base64 encoding and set `bodyEncoding` to `base64`.
The Lambda function should return a JSON object with a `type` of
`HTTPJSON-REP`, optional response metadata in `meta`, and the response body in
`body`. Response metadata can set the HTTP status and headers; `bodyEncoding`
can be set to `base64` for binary response bodies.

For `httpjson`, an incoming `X-Request-ID` header is preserved in
`meta.headers`; when it is absent, no synthetic ID is generated. When an ID is
present, it is used in structured logs and returned to the client in the
`X-Request-ID` response header.

### API Gateway v2

For API Gateway v2 events, request headers and duplicate query parameters use
API Gateway's comma-joined representation, while cookies are provided through 
the `cookies` list. The original path and query string are preserved in 
`rawPath` and `rawQueryString`.

Binary request bodies are base64-encoded and set `isBase64Encoded` to `true`; 
empty and text bodies are not base64-encoded.

Responses use the standard `statusCode`, `headers`, `cookies`, `body`, and 
`isBase64Encoded` fields. Each response cookie is emitted as a separate 
`Set-Cookie` header.

The API Gateway v2 adapter maps Caddy's request path directly to `rawPath`;
it does not trim a base path. AWS invocation errors, Lambda function
errors, timeouts, throttling, and malformed responses are returned as Caddy
handler errors.

For `api_gateway_v2`, an incoming `X-Request-ID` header is preserved; when it
is absent, caddy-lambda generates a UUID matching the format used by AWS API
Gateway and places it in `requestContext.requestId`. When an ID is present, it
is used in structured logs and returned to the client in the `X-Request-ID`
response header.

## Logging

HTTP access logging remains Caddy's responsibility. This module does not log
credentials, headers, or request/response bodies.

At `DEBUG` level, every attempted Lambda invocation emits the function name,
invocation duration, and a request ID. Failed
invocations also include the contextual error. At `INFO`, `WARN`, and `ERROR`
levels, the module does not emit a separate per-invocation record. Invocation
failures are returned as contextual handler errors for Caddy to log and convert
into an HTTP error response through its normal error handling configuration.

## Development

PRs and issues/bug reports are welcome!

If you'd like to develop locally, we use [mise](https://mise.jdx.dev/) commands 
to lint, format, and test the code.

```
mise lint
mise format
mise test
mise e2e
```

## Credits

This project is a fork of [floj/caddy-awslambda](github.com/floj/caddy-awslambda),
which is itself a port of [coopernurse/caddy-awslambda](https://github.com/coopernurse/caddy-awslambda),
based on a fork of of [abiosoft/caddy-exec](https://github.com/abiosoft/caddy-exec).

Many thanks to those authors and other contributors for their great contributions
to open source, without whom this project would not be possible.

## License

Apache 2
