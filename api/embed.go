package api

import _ "embed"

// OpenAPISpec is the embedded OpenAPI specification for the gateway.
//
//go:embed openapi.yaml
var OpenAPISpec []byte
