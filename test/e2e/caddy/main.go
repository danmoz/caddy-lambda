package main

import (
	"github.com/caddyserver/caddy/v2/cmd"
	_ "github.com/danmoz/caddy-lambda"
)

func main() {
	caddycmd.Main()
}
