# AWS Test Infrastructure

Sets up real AWS infrastructure to test this caddy plugin.

Running `mise aws-deploy` will:

- Build the python FastAPI test app into a lambda.zip deployment package
- Create a ECR docker repo service
- Build the container image locally with `xcaddy`
- Push the image to the ECR repo
- Set up an ECS cluster the deploys the image from ECR
- Sets up a lambda running the python service

The service intentionally HTTP-only for testing; so SSL.

The stack creates a VPC named `caddy-lambda-testing-vpc` with two public
subnets and an internet gateway. It does not require a default VPC.

## Prerequisites

- [mise](https://mise.jdx.dev/)
- Docker running locally
- AWS credentials in your shell (run `aws login`)

## Deploy

Trust the nested mise configuration once:

```sh
mise trust infra/mise.toml
```

Then run the commands from this directory:

```sh
cd infra
mise aws-deploy
```

The default region is `us-east-1`, configured as `AWS_REGION` in
`infra/mise.toml`. Override it and resource names with environment variables:

```sh
AWS_REGION=ap-southeast-2 mise aws-deploy
```

## Destroy

Remove the test resources, including the ECR repository and image:

```sh
mise aws-destroy
```
