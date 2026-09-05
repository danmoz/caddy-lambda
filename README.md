# caddy-lambda

Caddy v2 module for dispatching requests to AWS Lambda.

## Installation

```
xcaddy build \
    --with github.com/danmoz/caddy-lambda
```

## Usage

### Minimal configuration

The Lambda function name is required, and an AWS region must be available either
through the `region` setting or the AWS SDK's environment/shared configuration.

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

For production, prefer an IAM role attached to the Caddy runtime, or use
`role_arn` to assume a dedicated invocation role. The AWS SDK default
credential chain also supports environment variables, shared AWS configuration,
and runtime IAM roles; use environment or shared-file credentials primarily for
local development and protect them accordingly.

Lambda invocation uses Caddy's IAM identity; end-user authentication and
authorization remain the application's responsibility, with request headers
such as `Authorization` forwarded unchanged.

Please note that on invocation failure, the plugin will NOT retry. If an
apparent failure actually succeeded on AWS side, then a retry would cause
duplicate invocations.

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

### API Gateway v2

For API Gateway v2, request headers and duplicate query parameters use the
service's comma-joined representation, cookies use the `cookies` list, and the
raw path and query string are preserved in `rawPath` and `rawQueryString`.
Empty bodies are sent with base64 encoding disabled. Binary bodies use base64
encoding and set `isBase64Encoded` to `true`. Responses use `statusCode`,
`headers`, `cookies`, `body`, and `isBase64Encoded`; each response cookie is
emitted as a separate `Set-Cookie` header.

The API Gateway v2 adapter maps Caddy's request path directly to `rawPath`;
it does not trim a base path. AWS invocation errors, Lambda function
errors, timeouts, throttling, and malformed responses are returned as Caddy
handler errors and are not silently converted to successful responses.

## Logging

HTTP access logging remains Caddy's responsibility. This module does not log
credentials, headers, or request/response bodies.

At `DEBUG` level, every attempted Lambda invocation emits the function name,
invocation duration, and an incoming `X-Request-ID` when present. Failed
invocations also include the contextual error. At `INFO`, `WARN`, and `ERROR`
levels, the module does not emit a separate per-invocation record. Invocation
failures are returned as contextual handler errors for Caddy to log and convert
into an HTTP error response through its normal error handling configuration.

## Development

Use [mise](https://mise.jdx.dev/) commands to lint, format, and test the code.

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
