package main

import (
	"fmt"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestS2SecurityStaticContractChecks(t *testing.T) {
	c := testChecker(t)
	openapiPath := filepath.Join(c.protocolDir, "openapi.yaml")
	openapiDoc, err := readYAML(openapiPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.checkS2SecurityContract(openapiDoc); err != nil {
		t.Fatalf("S2 security checks failed: %v", err)
	}
}

func TestDeviceLookupCSRFContract(t *testing.T) {
	c := testChecker(t)
	schema := testSchema(t, c, "device-authorization-lookup.schema.json")

	t.Run("pending requires CSRF", func(t *testing.T) {
		fixture := testFixture(t, c, "device-auth-lookup.json")
		doc, err := asObject(fixture, "$")
		if err != nil {
			t.Fatal(err)
		}
		delete(doc, "csrf_token")
		if err := schema.Validate(doc); err == nil {
			t.Fatal("pending lookup accepted a response without csrf_token")
		}
	})

	t.Run("pending accepts CSRF", func(t *testing.T) {
		fixture := testFixture(t, c, "device-auth-lookup.json")
		if err := schema.Validate(fixture); err != nil {
			t.Fatalf("pending lookup fixture rejected: %v", err)
		}
	})

	t.Run("non-pending omits CSRF", func(t *testing.T) {
		fixture := testFixture(t, c, "device-auth-lookup.json")
		doc, err := asObject(fixture, "$")
		if err != nil {
			t.Fatal(err)
		}
		doc["state"] = "authorized"
		delete(doc, "csrf_token")
		if err := schema.Validate(doc); err != nil {
			t.Fatalf("non-pending lookup without csrf_token rejected: %v", err)
		}
	})

	t.Run("non-pending rejects CSRF", func(t *testing.T) {
		fixture := testFixture(t, c, "device-auth-lookup.json")
		doc, err := asObject(fixture, "$")
		if err != nil {
			t.Fatal(err)
		}
		doc["state"] = "authorized"
		if err := schema.Validate(doc); err == nil {
			t.Fatal("non-pending lookup accepted csrf_token")
		}
	})
}

func TestRefreshExpiresInContract(t *testing.T) {
	c := testChecker(t)
	schema := testSchema(t, c, "auth-token-pair.schema.json")

	for _, value := range []int64{0, 518400, 2592000} {
		t.Run(fmt.Sprintf("accepts_%d", value), func(t *testing.T) {
			fixture := testFixture(t, c, "device-auth-refresh.json")
			doc, err := asObject(fixture, "$")
			if err != nil {
				t.Fatal(err)
			}
			doc["refresh_expires_in"] = value
			if err := schema.Validate(doc); err != nil {
				t.Fatalf("refresh_expires_in=%d rejected: %v", value, err)
			}
		})
	}

	fixture := testFixture(t, c, "device-auth-refresh.json")
	doc, err := asObject(fixture, "$")
	if err != nil {
		t.Fatal(err)
	}
	doc["refresh_expires_in"] = 2592001
	if err := schema.Validate(doc); err == nil {
		t.Fatal("refresh_expires_in accepted a value beyond the sliding maximum")
	}
}

func TestS2SecurityFixturesUseExistingErrorCodes(t *testing.T) {
	c := testChecker(t)
	for _, name := range []string{"error-csrf-invalid.json", "error-email-verification-required.json"} {
		t.Run(name, func(t *testing.T) {
			fixture := testFixture(t, c, name)
			if err := c.checkFixtureSemantics(name, fixture); err != nil {
				t.Fatal(err)
			}
			if err := c.scanFixtureSafety(name, fixture); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestS3StaticContractChecks(t *testing.T) {
	c := testChecker(t)
	if err := c.checkS3Schemas(); err != nil {
		t.Fatalf("S3 schema checks failed: %v", err)
	}
}

func TestSafeTicketStatusRejectsTransportDescription(t *testing.T) {
	c := testChecker(t)
	schema := testSchema(t, c, "playback-ticket.schema.json")
	fixture := testFixture(t, c, "ticket-consuming-status.json")
	doc, err := asObject(fixture, "$")
	if err != nil {
		t.Fatal(err)
	}
	doc["transport"] = map[string]any{
		"kind": "direct-http",
		"streams": []any{
			map[string]any{
				"role":    "muxed",
				"url":     "https://source.example/example.mp4",
				"mime":    "video/mp4",
				"headers": map[string]any{},
				"range":   true,
			},
		},
	}
	if err := schema.Validate(doc); err == nil {
		t.Fatal("safe GET status schema accepted a transport description")
	}
}

func TestTicketCreateRequestRejectsSourceAndForceInputs(t *testing.T) {
	c := testChecker(t)
	schema := testSchema(t, c, "playback-ticket-create-request.schema.json")
	for _, field := range []string{"source_url", "source_headers", "mode", "burn", "force"} {
		t.Run(field, func(t *testing.T) {
			fixture := testFixture(t, c, "ticket-create-request.json")
			doc, err := asObject(fixture, "$")
			if err != nil {
				t.Fatal(err)
			}
			doc[field] = true
			if err := schema.Validate(doc); err == nil {
				t.Fatalf("create request accepted forbidden field %q", field)
			}
		})
	}
}

func TestS3TransportSetRejectsDashAndRelay(t *testing.T) {
	c := testChecker(t)
	schema := testSchema(t, c, "playback-ticket-issuance.schema.json")
	for _, kind := range []string{"direct-dash", "local-relay"} {
		t.Run(kind, func(t *testing.T) {
			fixture := testFixture(t, c, "ticket-direct-http.json")
			doc, err := asObject(fixture, "$")
			if err != nil {
				t.Fatal(err)
			}
			transport, err := asObject(doc["transport"], "$.transport")
			if err != nil {
				t.Fatal(err)
			}
			transport["kind"] = kind
			if err := schema.Validate(doc); err == nil {
				t.Fatalf("S3 issuance accepted excluded transport %q", kind)
			}
		})
	}
}

func TestPlaybackOperationsRequireDeadlineResponses(t *testing.T) {
	c := testChecker(t)
	for _, tc := range []struct {
		name   string
		path   string
		method string
	}{
		{name: "create", path: "/api/v1/playback/tickets", method: "post"},
		{name: "status", path: "/api/v1/playback/tickets/{id}", method: "get"},
		{name: "refresh", path: "/api/v1/playback/tickets/{id}/refresh", method: "post"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := readYAML(filepath.Join(c.protocolDir, "openapi.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			root, err := asObject(doc, "$")
			if err != nil {
				t.Fatal(err)
			}
			paths, err := requiredObject(root, "paths", "$")
			if err != nil {
				t.Fatal(err)
			}
			operation, err := playbackOperation(paths, tc.path, tc.method)
			if err != nil {
				t.Fatal(err)
			}
			responses, err := asObject(operation["responses"], tc.path+".responses")
			if err != nil {
				t.Fatal(err)
			}
			delete(responses, "408")
			if err := checkPlaybackDeadlineResponses(operation, tc.name); err == nil {
				t.Fatal("operation without 408 passed the deadline-response gate")
			}
		})
	}
}

func TestPlaybackHLSMethodPolicyRequiresHeadRejection(t *testing.T) {
	c := testChecker(t)
	doc, err := readYAML(filepath.Join(c.protocolDir, "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	root, err := asObject(doc, "$")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := requiredObject(root, "paths", "$")
	if err != nil {
		t.Fatal(err)
	}
	components, err := requiredObject(root, "components", "$")
	if err != nil {
		t.Fatal(err)
	}
	responses, err := requiredObject(components, "responses", "$.components")
	if err != nil {
		t.Fatal(err)
	}
	parameters, err := requiredObject(components, "parameters", "$.components")
	if err != nil {
		t.Fatal(err)
	}
	operation, err := playbackOperation(
		paths, "/api/v1/playback/media-sessions/{id}/index.m3u8", "get")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := requiredObject(
		operation,
		"x-http-method-policy",
		"$.paths[/api/v1/playback/media-sessions/{id}/index.m3u8].get")
	if err != nil {
		t.Fatal(err)
	}
	delete(policy, "HEAD_allow")
	if err := checkPlaybackHLSOperations(paths, responses, parameters); err == nil {
		t.Fatal("HLS method policy without HEAD_allow passed the protocol gate")
	}
}

func testChecker(t *testing.T) *checker {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test file path")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	return &checker{root: root, protocolDir: filepath.Join(root, "protocol")}
}

func testSchema(t *testing.T, c *checker, name string) *jsonschema.Schema {
	t.Helper()
	schemas, err := c.compileSchemas()
	if err != nil {
		t.Fatal(err)
	}
	schema, ok := schemas[name]
	if !ok {
		t.Fatalf("schema %s was not compiled", name)
	}
	return schema
}

func testFixture(t *testing.T, c *checker, name string) any {
	t.Helper()
	value, err := readJSON(filepath.Join(c.protocolDir, "fixtures", name))
	if err != nil {
		t.Fatal(err)
	}
	return value
}
