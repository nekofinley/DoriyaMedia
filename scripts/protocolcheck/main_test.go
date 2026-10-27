package main

import (
	"fmt"
	"path/filepath"
	"regexp"
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

func TestS5DirectDASHTransportContract(t *testing.T) {
	c := testChecker(t)
	schema := testSchema(t, c, "playback-ticket-issuance.schema.json")
	fixture := testFixture(t, c, "ticket-direct-dash.json")
	if err := schema.Validate(fixture); err != nil {
		t.Fatalf("direct-dash fixture rejected: %v", err)
	}

	for _, kind := range []string{"local-relay", "unknown-transport"} {
		t.Run(kind, func(t *testing.T) {
			mutated := testFixture(t, c, "ticket-direct-http.json")
			doc, err := asObject(mutated, "$")
			if err != nil {
				t.Fatal(err)
			}
			transport, err := asObject(doc["transport"], "$.transport")
			if err != nil {
				t.Fatal(err)
			}
			transport["kind"] = kind
			if err := schema.Validate(doc); err == nil {
				t.Fatalf("issuance accepted excluded transport %q", kind)
			}
		})
	}
}

func TestS5DirectDASHMutations(t *testing.T) {
	c := testChecker(t)
	schema := testSchema(t, c, "playback-ticket-issuance.schema.json")
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, map[string]any)
	}{
		{
			name: "missing audio",
			mutate: func(t *testing.T, doc map[string]any) {
				transport := nestedMap(t, doc, "transport")
				streams := ticketTransportStreams(t, doc)
				transport["streams"] = streams[:1]
			},
		},
		{
			name: "two video streams",
			mutate: func(t *testing.T, doc map[string]any) {
				streams := ticketTransportStreams(t, doc)
				audio := nestedMapFromValue(t, streams[1], "$.transport.streams[1]")
				audio["role"] = "video"
				audio["mime"] = "video/mp4"
			},
		},
		{
			name: "HTTP URL",
			mutate: func(t *testing.T, doc map[string]any) {
				streams := ticketTransportStreams(t, doc)
				video := nestedMapFromValue(t, streams[0], "$.transport.streams[0]")
				video["url"] = "http://video.example/dash/example-video.m4s"
			},
		},
		{
			name: "URL userinfo",
			mutate: func(t *testing.T, doc map[string]any) {
				streams := ticketTransportStreams(t, doc)
				video := nestedMapFromValue(t, streams[0], "$.transport.streams[0]")
				video["url"] = "https://user:password@video.example/dash/example-video.m4s"
			},
		},
		{
			name: "dangerous header",
			mutate: func(t *testing.T, doc map[string]any) {
				streams := ticketTransportStreams(t, doc)
				video := nestedMapFromValue(t, streams[0], "$.transport.streams[0]")
				headers, err := asObject(video["headers"], "$.transport.streams[0].headers")
				if err != nil {
					t.Fatal(err)
				}
				headers["Cookie"] = "session=example"
			},
		},
		{
			name: "media session",
			mutate: func(t *testing.T, doc map[string]any) {
				transport := nestedMap(t, doc, "transport")
				transport["media_session"] = map[string]any{
					"idle_expires_at":     "2026-10-26T12:20:00Z",
					"absolute_expires_at": "2026-10-26T18:05:00Z",
				}
			},
		},
		{
			name: "wrong MIME",
			mutate: func(t *testing.T, doc map[string]any) {
				streams := ticketTransportStreams(t, doc)
				video := nestedMapFromValue(t, streams[0], "$.transport.streams[0]")
				video["mime"] = "audio/mp4"
			},
		},
		{
			name: "range claimed",
			mutate: func(t *testing.T, doc map[string]any) {
				streams := ticketTransportStreams(t, doc)
				video := nestedMapFromValue(t, streams[0], "$.transport.streams[0]")
				video["range"] = true
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := testFixture(t, c, "ticket-direct-dash.json")
			doc, err := asObject(fixture, "$")
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, doc)
			if err := schema.Validate(doc); err == nil {
				t.Fatal("mutated direct-dash issuance passed the protocol gate")
			}
		})
	}
}

func TestS5DASHCapabilityAndTransportSet(t *testing.T) {
	c := testChecker(t)
	transportKind := testSchema(t, c, "playback-transport-kind.schema.json")
	for _, kind := range []string{
		"direct-http",
		"server-proxy",
		"server-hls",
		"direct-dash",
	} {
		if err := transportKind.Validate(kind); err != nil {
			t.Errorf("transport kind %q rejected: %v", kind, err)
		}
	}
	for _, kind := range []string{"local-relay", "unknown-transport"} {
		if err := transportKind.Validate(kind); err == nil {
			t.Errorf("transport kind %q accepted", kind)
		}
	}

	schema := testSchema(t, c, "playback-ticket-create-request.schema.json")
	for _, value := range []bool{false, true} {
		t.Run(fmt.Sprintf("supports_dash_merge_%t", value), func(t *testing.T) {
			fixture := testFixture(t, c, "ticket-create-request.json")
			doc, err := asObject(fixture, "$")
			if err != nil {
				t.Fatal(err)
			}
			client := nestedMap(t, doc, "client")
			client["supports_dash_merge"] = value
			if err := schema.Validate(doc); err != nil {
				t.Fatalf("boolean supports_dash_merge rejected: %v", err)
			}
		})
	}

	t.Run("non-boolean capability", func(t *testing.T) {
		fixture := testFixture(t, c, "ticket-create-request.json")
		doc, err := asObject(fixture, "$")
		if err != nil {
			t.Fatal(err)
		}
		nestedMap(t, doc, "client")["supports_dash_merge"] = "true"
		if err := schema.Validate(doc); err == nil {
			t.Fatal("non-boolean supports_dash_merge accepted")
		}
	})

	t.Run("four preferred transports", func(t *testing.T) {
		fixture := testFixture(t, c, "ticket-create-request.json")
		doc, err := asObject(fixture, "$")
		if err != nil {
			t.Fatal(err)
		}
		client := nestedMap(t, doc, "client")
		client["supports_dash_merge"] = true
		doc["preferred_transports"] = []any{
			"direct-dash",
			"server-hls",
			"direct-http",
			"server-proxy",
		}
		if err := schema.Validate(doc); err != nil {
			t.Fatalf("four preferred transports rejected: %v", err)
		}
	})

	t.Run("more than four preferred transports", func(t *testing.T) {
		fixture := testFixture(t, c, "ticket-create-request.json")
		doc, err := asObject(fixture, "$")
		if err != nil {
			t.Fatal(err)
		}
		doc["preferred_transports"] = []any{
			"direct-http",
			"server-proxy",
			"server-hls",
			"direct-dash",
			"server-hls",
		}
		if err := schema.Validate(doc); err == nil {
			t.Fatal("more than four preferred transports accepted")
		}
	})
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

func TestS4MediaAndRangeContract(t *testing.T) {
	c := testChecker(t)
	doc := testOpenAPI(t, c)
	if err := checkPlaybackMediaContract(doc); err != nil {
		t.Fatalf("media response matrix failed: %v", err)
	}
	if err := checkPlaybackRangeContract(doc); err != nil {
		t.Fatalf("range contract failed: %v", err)
	}
}

func TestS4MediaResponseMatrixMutations(t *testing.T) {
	streamPath := "/api/v1/playback/media-sessions/{id}/stream"
	playlistPath := "/api/v1/playback/media-sessions/{id}/index.m3u8"
	segmentPath := "/api/v1/playback/media-sessions/{id}/{segment}"

	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, map[string]any)
	}{
		{
			name: "stream GET loses 400",
			mutate: func(t *testing.T, root map[string]any) {
				delete(nestedMap(t, root, "paths", streamPath, "get", "responses"), "400")
			},
		},
		{
			name: "stream HEAD loses 502",
			mutate: func(t *testing.T, root map[string]any) {
				delete(nestedMap(t, root, "paths", streamPath, "head", "responses"), "502")
			},
		},
		{
			name: "stream HEAD loses 504",
			mutate: func(t *testing.T, root map[string]any) {
				delete(nestedMap(t, root, "paths", streamPath, "head", "responses"), "504")
			},
		},
		{
			name: "stream GET loses 408",
			mutate: func(t *testing.T, root map[string]any) {
				delete(nestedMap(t, root, "paths", streamPath, "get", "responses"), "408")
			},
		},
		{
			name: "HLS playlist loses 504",
			mutate: func(t *testing.T, root map[string]any) {
				delete(nestedMap(t, root, "paths", playlistPath, "get", "responses"), "504")
			},
		},
		{
			name: "HLS playlist loses 502",
			mutate: func(t *testing.T, root map[string]any) {
				delete(nestedMap(t, root, "paths", playlistPath, "get", "responses"), "502")
			},
		},
		{
			name: "HLS segment loses 408",
			mutate: func(t *testing.T, root map[string]any) {
				delete(nestedMap(t, root, "paths", segmentPath, "get", "responses"), "408")
			},
		},
		{
			name: "HEAD gains a body",
			mutate: func(t *testing.T, root map[string]any) {
				nestedMap(t, root, "paths", streamPath, "head", "responses", "200")["content"] =
					map[string]any{"application/json": map[string]any{}}
			},
		},
		{
			name: "HEAD references a bodyful response",
			mutate: func(t *testing.T, root map[string]any) {
				nestedMap(t, root, "paths", streamPath, "head", "responses", "502")["$ref"] =
					"#/components/responses/StableError"
			},
		},
		{
			name: "GET error drops stable schema",
			mutate: func(t *testing.T, root map[string]any) {
				nestedMap(t, root, "paths", streamPath, "get", "responses", "502")["$ref"] =
					"#/components/responses/ServerProxyStream"
			},
		},
		{
			name: "HEAD range request loses IfRange",
			mutate: func(t *testing.T, root map[string]any) {
				nestedMap(t, root, "paths", streamPath, "head")["parameters"] = []any{
					map[string]any{"$ref": "#/components/parameters/MediaSessionId"},
					map[string]any{"$ref": "#/components/parameters/Range"},
				}
			},
		},
		{
			name: "HLS extension drops 408 code",
			mutate: func(t *testing.T, root map[string]any) {
				delete(nestedMap(t, root, "x-server-hls-media-session", "stable_errors"), "408")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testChecker(t)
			root := testOpenAPI(t, c)
			tc.mutate(t, root)
			if err := checkPlaybackMediaContract(root); err == nil {
				t.Fatal("mutated media response matrix passed the protocol gate")
			}
		})
	}
}

func TestS4RangeContractMutations(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, map[string]any)
	}{
		{
			name: "Range pattern loses outer OWS",
			mutate: func(t *testing.T, root map[string]any) {
				nestedMap(t, root, "components", "parameters", "Range", "schema")["pattern"] =
					"^bytes=[0-9]+-[0-9]*$"
			},
		},
		{
			name: "Range no longer rejects suffix",
			mutate: func(t *testing.T, root map[string]any) {
				nestedMap(t, root, "components", "parameters", "Range")["x-rejected-forms"] =
					[]any{"multi-range", "repeated-header", "reversed"}
			},
		},
		{
			name: "Range no longer rejects reverse",
			mutate: func(t *testing.T, root map[string]any) {
				nestedMap(t, root, "components", "parameters", "Range")["x-rejected-forms"] =
					[]any{"suffix", "multi-range", "repeated-header"}
			},
		},
		{
			name: "Range no longer rejects multi-range",
			mutate: func(t *testing.T, root map[string]any) {
				nestedMap(t, root, "components", "parameters", "Range")["x-rejected-forms"] =
					[]any{"suffix", "repeated-header", "reversed"}
			},
		},
		{
			name: "Range no longer accepts open interval",
			mutate: func(t *testing.T, root map[string]any) {
				nestedMap(t, root, "components", "parameters", "Range")["x-accepted-forms"] =
					[]any{"closed"}
			},
		},
		{
			name: "Range no longer accepts closed interval",
			mutate: func(t *testing.T, root map[string]any) {
				nestedMap(t, root, "components", "parameters", "Range")["x-accepted-forms"] =
					[]any{"open-ended"}
			},
		},
		{
			name: "206 Content-Range becomes optional",
			mutate: func(t *testing.T, root map[string]any) {
				nestedMap(t, root, "components", "responses", "ServerProxyPartialContent",
					"headers", "Content-Range")["x-required-on-status"] = 200
			},
		},
		{
			name: "HEAD 206 Content-Range becomes optional",
			mutate: func(t *testing.T, root map[string]any) {
				nestedMap(t, root, "paths", "/api/v1/playback/media-sessions/{id}/stream",
					"head", "responses", "206", "headers", "Content-Range")["x-required-on-status"] =
					200
			},
		},
		{
			name: "206 Content-Range pattern weakens",
			mutate: func(t *testing.T, root map[string]any) {
				nestedMap(t, root, "components", "responses", "ServerProxyPartialContent",
					"headers", "Content-Range", "schema")["pattern"] = "^bytes .*$"
			},
		},
		{
			name: "partial response loses ETag",
			mutate: func(t *testing.T, root map[string]any) {
				delete(nestedMap(t, root, "components", "responses", "ServerProxyPartialContent",
					"headers"), "ETag")
			},
		},
		{
			name: "ETag allows weak If-Range",
			mutate: func(t *testing.T, root map[string]any) {
				nestedMap(t, root, "components", "responses", "ServerProxyStream",
					"headers", "ETag")["x-if-range-validator"] = "any"
			},
		},
		{
			name: "If-Range becomes a free-form validator",
			mutate: func(t *testing.T, root map[string]any) {
				nestedMap(t, root, "components", "parameters", "IfRange")["x-if-range-accepted"] =
					"any"
			},
		},
		{
			name: "If-Range accepted types lose HTTP-date",
			mutate: func(t *testing.T, root map[string]any) {
				nestedMap(t, root, "components", "parameters", "IfRange")["x-if-range-accepted"] =
					"exact-strong-etag"
			},
		},
		{
			name: "If-Range local matching stops requiring exact strong ETag",
			mutate: func(t *testing.T, root map[string]any) {
				nestedMap(t, root, "components", "parameters", "IfRange")["x-local-exact-strong-etag-required"] =
					false
			},
		},
		{
			name: "If-Range reintroduces obsolete HTTP-date flag",
			mutate: func(t *testing.T, root map[string]any) {
				nestedMap(t, root, "components", "parameters", "IfRange")["x-if-range-accepted-http-date"] =
					true
			},
		},
		{
			name: "If-Range schema becomes non-string",
			mutate: func(t *testing.T, root map[string]any) {
				nestedMap(t, root, "components", "parameters", "IfRange",
					"schema")["type"] = "integer"
			},
		},
		{
			name: "local If-Range mismatch changes status",
			mutate: func(t *testing.T, root map[string]any) {
				nestedMap(t, root, "components", "parameters", "IfRange")["x-local-nonmatch-status"] =
					206
			},
		},
		{
			name: "server-proxy conflicting partial changes status",
			mutate: func(t *testing.T, root map[string]any) {
				nestedMap(t, root, "components", "parameters", "IfRange")["x-server-proxy-conflict-status"] =
					400
			},
		},
		{
			name: "range contract loses validator rules",
			mutate: func(t *testing.T, root map[string]any) {
				delete(nestedMap(t, root, "x-playback-byte-ranges"), "validators")
			},
		},
		{
			name: "ETag schema becomes non-string",
			mutate: func(t *testing.T, root map[string]any) {
				nestedMap(t, root, "components", "responses", "ServerProxyStream",
					"headers", "ETag", "schema")["type"] = "integer"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testChecker(t)
			root := testOpenAPI(t, c)
			tc.mutate(t, root)
			if err := checkPlaybackRangeContract(root); err == nil {
				t.Fatal("mutated range contract passed the protocol gate")
			}
		})
	}
}

func TestS4RangePatternSamples(t *testing.T) {
	pattern := regexp.MustCompile(
		`^[ \t]*[bB][yY][tT][eE][sS]=[0-9]+-(?:[0-9]+)?[ \t]*$`)
	for _, value := range []string{
		" bytes=0-0 ",
		"\tBYTES=10-\t",
		"bytes=123-456",
	} {
		if !pattern.MatchString(value) {
			t.Errorf("Range pattern rejected %q", value)
		}
	}
	for _, value := range []string{
		"bytes=-10",
		"bytes=0-1,3-4",
		"bytes=10 -20",
		"bytes=10-20,30-40",
	} {
		if pattern.MatchString(value) {
			t.Errorf("Range pattern accepted %q", value)
		}
	}
}

func TestS4PlaybackFailureCodeContract(t *testing.T) {
	c := testChecker(t)
	if err := c.checkPlaybackFailureCodeContract(testOpenAPI(t, c)); err != nil {
		t.Fatalf("failure-code contract failed: %v", err)
	}
}

func TestS4PlaybackFailureCodeMutations(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, map[string]any, map[string]any, map[string]any, map[string]any)
	}{
		{
			name: "OpenAPI drops probe_timeout",
			mutate: func(t *testing.T, openapi, _, _, _ map[string]any) {
				openapi["x-playback-failure-codes"].(map[string]any)["failed"] =
					[]any{"source_unavailable", "probe_interrupted", "probe_finalize_failed"}
			},
		},
		{
			name: "unknown internal fallback leaks",
			mutate: func(t *testing.T, openapi, _, _, _ map[string]any) {
				openapi["x-playback-failure-codes"].(map[string]any)["unknown_internal_failed_fallback"] =
					"internal_error"
			},
		},
		{
			name: "closed schema drops probe_interrupted",
			mutate: func(t *testing.T, _, failureCode, _, _ map[string]any) {
				failureCode["enum"] = []any{
					"source_unavailable",
					"probe_timeout",
					"probe_finalize_failed",
					"unsupported_media",
				}
			},
		},
		{
			name: "status drops probe_finalize_failed",
			mutate: func(t *testing.T, _, _, status, _ map[string]any) {
				constraint, err := failureCodeConstraint(status, "failed")
				if err != nil {
					t.Fatal(err)
				}
				constraint["enum"] = []any{
					"source_unavailable",
					"probe_timeout",
					"probe_interrupted",
				}
			},
		},
		{
			name: "issuance changes failure schema",
			mutate: func(t *testing.T, _, _, _, issuance map[string]any) {
				properties := nestedMap(t, issuance, "properties")
				properties["failure_code"] = map[string]any{
					"$ref": "playback-ticket.schema.json",
				}
			},
		},
		{
			name: "ready status may carry a failure code",
			mutate: func(t *testing.T, _, _, status, _ map[string]any) {
				constraint, _, err := failureCodeForbiddenConstraint(status)
				if err != nil {
					t.Fatal(err)
				}
				constraint["failure_code"] = true
			},
		},
		{
			name: "issuance may carry a failure code while ready",
			mutate: func(t *testing.T, _, _, _, issuance map[string]any) {
				constraint, _, err := failureCodeForbiddenConstraint(issuance)
				if err != nil {
					t.Fatal(err)
				}
				delete(constraint, "failure_code")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testChecker(t)
			openapi := testOpenAPI(t, c)
			failureCode := testProtocolJSON(t, c, "playback-failure-code.schema.json")
			status := testProtocolJSON(t, c, "playback-ticket.schema.json")
			issuance := testProtocolJSON(t, c, "playback-ticket-issuance.schema.json")
			tc.mutate(t, openapi, failureCode, status, issuance)
			if err := checkPlaybackFailureCodeDocs(
				openapi, failureCode, status, issuance); err == nil {
				t.Fatal("mutated failure-code contract passed the protocol gate")
			}
		})
	}
}

func TestS4FailureCodeFixturesMatchDeclaredCodes(t *testing.T) {
	c := testChecker(t)
	for name, expectation := range expectedTickets {
		if expectation.failureCode == "" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			fixture := testFixture(t, c, name)
			if err := checkTicketFixture(name, fixture); err != nil {
				t.Fatalf("fixture failed semantic check: %v", err)
			}
			doc, err := asObject(fixture, "$")
			if err != nil {
				t.Fatal(err)
			}
			doc["failure_code"] = "internal_detail_canary"
			if err := checkTicketFixture(name, doc); err == nil {
				t.Fatal("fixture checker accepted an internal failure code")
			}
		})
	}
}

func TestFailedTicketRejectsUnknownInternalFailureCode(t *testing.T) {
	c := testChecker(t)
	schema := testSchema(t, c, "playback-ticket.schema.json")
	fixture := testFixture(t, c, "ticket-failed.json")
	doc, err := asObject(fixture, "$")
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{
		"source_unavailable",
		"probe_timeout",
		"probe_interrupted",
		"probe_finalize_failed",
	} {
		doc["failure_code"] = code
		if err := schema.Validate(doc); err != nil {
			t.Errorf("known failed code %q rejected: %v", code, err)
		}
	}
	doc["failure_code"] = "internal_probe_detail_canary"
	if err := schema.Validate(doc); err == nil {
		t.Fatal("failed ticket exposed an unknown internal failure code")
	}
	doc["failure_code"] = "unsupported_media"
	if err := schema.Validate(doc); err == nil {
		t.Fatal("failed ticket accepted the unsupported-only failure code")
	}
}

func TestFailureCodeIsRejectedOutsideTerminalFailureStates(t *testing.T) {
	c := testChecker(t)
	schema := testSchema(t, c, "playback-ticket.schema.json")
	fixture := testFixture(t, c, "ticket-ready.json")
	doc, err := asObject(fixture, "$")
	if err != nil {
		t.Fatal(err)
	}
	doc["failure_code"] = "probe_timeout"
	if err := schema.Validate(doc); err == nil {
		t.Fatal("ready ticket accepted a failure_code")
	}
}

func TestComponentSchemaSetIsClosed(t *testing.T) {
	c := testChecker(t)
	root := testOpenAPI(t, c)
	if err := c.checkOpenAPIShape(root); err != nil {
		t.Fatalf("baseline OpenAPI failed: %v", err)
	}
	delete(nestedMap(t, root, "components", "schemas"), "PlaybackFailureCode")
	if err := c.checkOpenAPIShape(root); err == nil {
		t.Fatal("OpenAPI passed after deleting the PlaybackFailureCode component")
	}
}

func TestCreateErrorMustDocumentRecoverableLocation(t *testing.T) {
	c := testChecker(t)
	root := testOpenAPI(t, c)
	components := nestedMap(t, root, "components")
	paths := nestedMap(t, root, "paths")
	if err := checkPlaybackOperations(root, paths, components); err != nil {
		t.Fatalf("baseline playback operations failed: %v", err)
	}
	responses := nestedMap(t, root, "paths", "/api/v1/playback/tickets",
		"post", "responses")
	delete(nestedMap(t, responses, "500"), "headers")
	if err := checkPlaybackOperations(root, paths, components); err == nil {
		t.Fatal("create 500 passed without its recoverable Location header")
	}
}

func testOpenAPI(t *testing.T, c *checker) map[string]any {
	t.Helper()
	value, err := readYAML(filepath.Join(c.protocolDir, "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	root, err := asObject(value, "$")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func testProtocolJSON(t *testing.T, c *checker, name string) map[string]any {
	t.Helper()
	value, err := readJSON(filepath.Join(c.protocolDir, "schemas", name))
	if err != nil {
		t.Fatal(err)
	}
	root, err := asObject(value, "$")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func nestedMap(t *testing.T, root map[string]any, keys ...string) map[string]any {
	t.Helper()
	current := root
	for i, key := range keys {
		next, err := requiredObject(current, key,
			fmt.Sprintf("$.%s", joinPath(keys[:i+1])))
		if err != nil {
			t.Fatal(err)
		}
		current = next
	}
	return current
}

func nestedMapFromValue(t *testing.T, value any, path string) map[string]any {
	t.Helper()
	object, err := asObject(value, path)
	if err != nil {
		t.Fatal(err)
	}
	return object
}

func ticketTransportStreams(t *testing.T, doc map[string]any) []any {
	t.Helper()
	transport := nestedMap(t, doc, "transport")
	streams, err := asArray(transport["streams"], "$.transport.streams")
	if err != nil {
		t.Fatal(err)
	}
	return streams
}

func joinPath(parts []string) string {
	out := ""
	for _, part := range parts {
		if out != "" {
			out += "."
		}
		out += part
	}
	return out
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
