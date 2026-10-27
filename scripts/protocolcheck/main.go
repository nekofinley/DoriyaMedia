package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

var requiredFixtures = []string{
	"meta.json",
	"device-auth-start.json",
	"device-auth-pending.json",
	"device-auth-authorized.json",
	"device-auth-lookup.json",
	"device-auth-decision.json",
	"device-auth-token-pending.json",
	"device-auth-token.json",
	"device-auth-refresh.json",
	"device-list.json",
	"device-revoked.json",
	"ticket-create-request.json",
	"ticket-probing.json",
	"ticket-ready.json",
	"ticket-consuming-status.json",
	"ticket-direct-http.json",
	"ticket-server-proxy.json",
	"ticket-server-hls.json",
	"ticket-direct-dash.json",
	"ticket-unsupported.json",
	"ticket-failed.json",
	"ticket-failed-probe-timeout.json",
	"ticket-failed-probe-interrupted.json",
	"ticket-failed-probe-finalize-failed.json",
	"ticket-expired.json",
	"ticket-revoked.json",
	"ticket-exhausted.json",
	"media-session.json",
	"subtitle-vtt.json",
	"subtitle-pgs.json",
	"error-client-upgrade-required.json",
	"error-device-revoked.json",
	"error-ticket-expired.json",
	"error-ticket-probing.json",
	"error-ticket-revoked.json",
	"error-ticket-not-found.json",
	"error-unsupported-media.json",
	"error-source-unavailable.json",
	"error-transport-disabled.json",
	"error-media-session-not-found.json",
	"error-media-session-expired.json",
	"error-rate-limited.json",
	"error-authorization-denied.json",
	"error-authorization-expired.json",
	"error-authorization-consumed.json",
	"error-refresh-invalid.json",
	"error-refresh-reuse.json",
	"error-csrf-invalid.json",
	"error-email-verification-required.json",
}

var requiredSchemas = []string{
	"meta.schema.json",
	"error.schema.json",
	"device-platform.schema.json",
	"device-authorization-state.schema.json",
	"device-authorization-start-request.schema.json",
	"device-authorization-start.schema.json",
	"device-authorization-status.schema.json",
	"device-authorization-lookup.schema.json",
	"device-authorization-decision-request.schema.json",
	"device-authorization-decision.schema.json",
	"device-token-request.schema.json",
	"device-token-pending.schema.json",
	"auth-token-pair.schema.json",
	"refresh-token-request.schema.json",
	"device.schema.json",
	"device-list.schema.json",
	"device-revocation.schema.json",
	"auth-user.schema.json",
	"stream-headers.schema.json",
	"stream.schema.json",
	"media.schema.json",
	"media-session.schema.json",
	"subtitle.schema.json",
	"transport.schema.json",
	"transport-direct-http.schema.json",
	"direct-dash-stream.schema.json",
	"transport-direct-dash.schema.json",
	"transport-server-proxy.schema.json",
	"transport-server-hls.schema.json",
	"playback-ticket.schema.json",
	"playback-ticket-issuance.schema.json",
	"playback-failure-code.schema.json",
	"playback-ticket-create-request.schema.json",
	"playback-ticket-state.schema.json",
	"playback-transport-kind.schema.json",
	"playback-client-capabilities.schema.json",
}

var expectedErrors = map[string]string{
	"error-client-upgrade-required.json":     "client_upgrade_required",
	"error-device-revoked.json":              "device_revoked",
	"error-ticket-expired.json":              "ticket_expired",
	"error-ticket-probing.json":              "invalid_request",
	"error-ticket-revoked.json":              "forbidden",
	"error-ticket-not-found.json":            "ticket_not_found",
	"error-unsupported-media.json":           "unsupported_media",
	"error-source-unavailable.json":          "source_unavailable",
	"error-transport-disabled.json":          "transport_disabled",
	"error-media-session-not-found.json":     "media_session_not_found",
	"error-media-session-expired.json":       "media_session_expired",
	"error-rate-limited.json":                "rate_limited",
	"error-authorization-denied.json":        "authorization_denied",
	"error-authorization-expired.json":       "authorization_expired",
	"error-authorization-consumed.json":      "authorization_consumed",
	"error-refresh-invalid.json":             "refresh_token_invalid",
	"error-refresh-reuse.json":               "refresh_reuse_detected",
	"error-csrf-invalid.json":                "forbidden",
	"error-email-verification-required.json": "forbidden",
}

var fixtureUserCode = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{5}-[0-9A-HJKMNP-TV-Z]{5}$`)

type operationExpectation struct {
	path     string
	method   string
	security string
}

var expectedOperations = []operationExpectation{
	{path: "/api/v1/meta", method: "get", security: "public"},
	{path: "/api/v1/auth/device/authorizations", method: "post", security: "public"},
	{path: "/api/v1/auth/device/authorizations/{id}", method: "get", security: "public"},
	{path: "/api/v1/auth/device/authorizations/lookup", method: "get", security: "webSession"},
	{path: "/api/v1/auth/device/authorizations/{id}/decision", method: "post", security: "webSession"},
	{path: "/api/v1/auth/device/token", method: "post", security: "public"},
	{path: "/api/v1/auth/refresh", method: "post", security: "public"},
	{path: "/api/v1/auth/logout", method: "post", security: "bearerAuth"},
	{path: "/api/v1/devices", method: "get", security: "bearerAuth"},
	{path: "/api/v1/devices/{id}", method: "delete", security: "bearerAuth"},
	{path: "/api/v1/playback/tickets", method: "post", security: "bearerAuth"},
	{path: "/api/v1/playback/tickets/{id}", method: "get", security: "bearerAuth"},
	{path: "/api/v1/playback/tickets/{id}/refresh", method: "post", security: "bearerAuth"},
	{path: "/api/v1/playback/media-sessions/{id}/stream", method: "get", security: "mediaCapability"},
	{path: "/api/v1/playback/media-sessions/{id}/stream", method: "head", security: "mediaCapability"},
	{path: "/api/v1/playback/media-sessions/{id}/index.m3u8", method: "get", security: "mediaCapability"},
	{path: "/api/v1/playback/media-sessions/{id}/{segment}", method: "get", security: "mediaCapability"},
	{path: "/api/v1/playback/subtitle-sessions/{id}/subtitle.vtt", method: "get", security: "subtitleCapability"},
	{path: "/api/v1/playback/subtitle-sessions/{id}/subtitle.vtt", method: "head", security: "subtitleCapability"},
	{path: "/api/v1/playback/media-sessions/{id}/subtitle-sessions/{subtitle}", method: "get", security: "mediaAndSubtitleCapability"},
	{path: "/api/v1/playback/media-sessions/{id}/subtitle-sessions/{subtitle}", method: "head", security: "mediaAndSubtitleCapability"},
}

var expectedAuthorizationStates = map[string]string{
	"device-auth-pending.json":       "pending",
	"device-auth-authorized.json":    "authorized",
	"device-auth-lookup.json":        "pending",
	"device-auth-decision.json":      "authorized",
	"device-auth-token-pending.json": "pending",
}

type ticketExpectation struct {
	schema      string
	state       string
	kind        string
	failureCode string
}

var expectedTickets = map[string]ticketExpectation{
	"ticket-probing.json":          {schema: "status", state: "probing"},
	"ticket-ready.json":            {schema: "status", state: "ready"},
	"ticket-consuming-status.json": {schema: "status", state: "consuming"},
	"ticket-direct-http.json":      {schema: "issuance", state: "consuming", kind: "direct-http"},
	"ticket-server-proxy.json":     {schema: "issuance", state: "consuming", kind: "server-proxy"},
	"ticket-server-hls.json":       {schema: "issuance", state: "consuming", kind: "server-hls"},
	"ticket-direct-dash.json":      {schema: "issuance", state: "consuming", kind: "direct-dash"},
	"ticket-unsupported.json":      {schema: "status", state: "unsupported", failureCode: "unsupported_media"},
	"ticket-failed.json":           {schema: "status", state: "failed", failureCode: "source_unavailable"},
	"ticket-failed-probe-timeout.json": {
		schema: "status", state: "failed", failureCode: "probe_timeout",
	},
	"ticket-failed-probe-interrupted.json": {
		schema: "status", state: "failed", failureCode: "probe_interrupted",
	},
	"ticket-failed-probe-finalize-failed.json": {
		schema: "status", state: "failed", failureCode: "probe_finalize_failed",
	},
	"ticket-expired.json":   {schema: "status", state: "expired"},
	"ticket-revoked.json":   {schema: "status", state: "revoked"},
	"ticket-exhausted.json": {schema: "status", state: "exhausted"},
}

var requiredTicketStates = []string{
	"probing",
	"ready",
	"consuming",
	"unsupported",
	"failed",
	"expired",
	"revoked",
	"exhausted",
}

var requiredErrorCodes = []string{
	"transport_disabled",
	"ticket_not_found",
	"ticket_expired",
	"media_session_not_found",
	"media_session_expired",
}

var forbiddenPlaybackRequestKeys = []string{
	"authorization",
	"burn",
	"cookie",
	"direct_url",
	"force",
	"header",
	"headers",
	"mode",
	"query",
	"session",
	"source",
	"source_headers",
	"source_url",
	"token",
	"url",
}

var (
	bearerValue = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{12,}`)
	basicValue  = regexp.MustCompile(`(?i)\bbasic\s+[A-Za-z0-9+/=]{8,}`)
	jwtValue    = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)
	secretQuery = regexp.MustCompile(`(?i)[?&](?:sig|signature|x-signature|token|access_token|refresh_token|auth|expires|exp|key|api_key)=`)
)

type checker struct {
	root         string
	protocolDir  string
	refCount     int
	fixtureCount int
	schemaCount  int
}

type refOccurrence struct {
	owner string
	path  string
	ref   string
}

func main() {
	rootFlag := flag.String("root", ".", "repository root")
	flag.Parse()

	root, err := filepath.Abs(*rootFlag)
	if err != nil {
		fail(err)
	}
	c := &checker{
		root:        root,
		protocolDir: filepath.Join(root, "protocol"),
	}
	if err := c.run(); err != nil {
		fail(err)
	}

	fmt.Printf(
		"protocol check passed: %d schemas, %d fixtures, %d refs\n",
		c.schemaCount,
		c.fixtureCount,
		c.refCount,
	)
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "protocol check FAILED\n%s\n", err)
	os.Exit(1)
}

func (c *checker) run() error {
	if err := c.checkRequiredFiles(); err != nil {
		return err
	}

	openapiPath := filepath.Join(c.protocolDir, "openapi.yaml")
	openapiDoc, err := readYAML(openapiPath)
	if err != nil {
		return err
	}
	if err := c.checkOpenAPIShape(openapiDoc); err != nil {
		return fmt.Errorf("openapi.yaml: %w", err)
	}
	if err := c.checkS2SecurityContract(openapiDoc); err != nil {
		return fmt.Errorf("S2 security contract: %w", err)
	}
	if err := c.checkRefs(openapiPath, openapiDoc); err != nil {
		return err
	}
	if err := c.checkS3Schemas(); err != nil {
		return err
	}
	if err := checkPlaybackMediaContract(openapiDoc); err != nil {
		return fmt.Errorf("S4 media contract: %w", err)
	}
	if err := checkPlaybackRangeContract(openapiDoc); err != nil {
		return fmt.Errorf("S4 range contract: %w", err)
	}
	if err := c.checkPlaybackFailureCodeContract(openapiDoc); err != nil {
		return fmt.Errorf("S4 failure-code contract: %w", err)
	}

	schemas, err := c.compileSchemas()
	if err != nil {
		return err
	}
	return c.checkFixtures(schemas)
}

func (c *checker) checkS2SecurityContract(value any) error {
	doc, err := asObject(value, "$")
	if err != nil {
		return err
	}
	lifetimes, err := requiredObject(doc, "x-device-token-lifetimes", "$")
	if err != nil {
		return err
	}
	for key, want := range map[string]int64{
		"access_token_seconds":            900,
		"refresh_sliding_seconds":         2592000,
		"refresh_family_absolute_seconds": 15552000,
	} {
		got, err := asInteger(lifetimes[key], "$.x-device-token-lifetimes."+key)
		if err != nil || got != want {
			return fmt.Errorf("$.x-device-token-lifetimes.%s must be %d", key, want)
		}
	}
	if rule, _ := lifetimes["refresh_expires_in_rule"].(string); !strings.Contains(rule, "dynamic") {
		return fmt.Errorf("$.x-device-token-lifetimes.refresh_expires_in_rule must state that refresh_expires_in is dynamic")
	}

	security, err := requiredObject(doc, "x-device-authorization-security", "$")
	if err != nil {
		return err
	}
	for key, want := range map[string]string{
		"lookup_cache_control": "no-store",
		"csrf_field":           "csrf_token",
		"decision_origin":      "exact-scheme-and-host",
		"csrf_header":          "X-CSRF-Token",
	} {
		if got, _ := security[key].(string); got != want {
			return fmt.Errorf("$.x-device-authorization-security.%s must be %q", key, want)
		}
	}
	if ttl, err := asInteger(security["csrf_ttl_seconds"], "$.x-device-authorization-security.csrf_ttl_seconds"); err != nil || ttl != 300 {
		return fmt.Errorf("$.x-device-authorization-security.csrf_ttl_seconds must be 300")
	}
	if rotate, ok := security["csrf_rotates_on_pending_lookup"].(bool); !ok || !rotate {
		return fmt.Errorf("$.x-device-authorization-security.csrf_rotates_on_pending_lookup must be true")
	}
	fetchSite, err := requiredObject(security, "fetch_site", "$.x-device-authorization-security")
	if err != nil {
		return err
	}
	if header, _ := fetchSite["header"].(string); header != "Sec-Fetch-Site" {
		return fmt.Errorf("$.x-device-authorization-security.fetch_site.header must be Sec-Fetch-Site")
	}
	if required, ok := fetchSite["required"].(bool); !ok || required {
		return fmt.Errorf("$.x-device-authorization-security.fetch_site.required must be false")
	}
	if allowed, _ := fetchSite["allowed"].(string); allowed != "same-origin" {
		return fmt.Errorf("$.x-device-authorization-security.fetch_site.allowed must be same-origin")
	}
	csrfFailure, err := requiredObject(security, "csrf_failure", "$.x-device-authorization-security")
	if err != nil {
		return err
	}
	if err := checkStatusAndError(csrfFailure, 403, "forbidden", "reload_authorization", "$.x-device-authorization-security.csrf_failure"); err != nil {
		return err
	}
	consumedReplay, err := requiredObject(security, "consumed_decision_replay", "$.x-device-authorization-security")
	if err != nil {
		return err
	}
	if err := checkStatusAndError(consumedReplay, 409, "authorization_consumed", "", "$.x-device-authorization-security.consumed_decision_replay"); err != nil {
		return err
	}

	emailGate, err := requiredObject(doc, "x-email-verification-gate", "$")
	if err != nil {
		return err
	}
	if condition, _ := emailGate["condition"].(string); condition != "required-by-config-and-nonempty-unverified-email" {
		return fmt.Errorf("$.x-email-verification-gate.condition is invalid")
	}
	if exempt, ok := emailGate["empty_email_exempt"].(bool); !ok || !exempt {
		return fmt.Errorf("$.x-email-verification-gate.empty_email_exempt must be true")
	}
	for _, expectation := range []struct {
		statusKey string
		codeKey   string
		status    int64
		code      string
	}{
		{statusKey: "lookup_decision_status", codeKey: "lookup_decision_code", status: 401, code: "unauthenticated"},
		{statusKey: "token_exchange_status", codeKey: "token_exchange_code", status: 403, code: "forbidden"},
		{statusKey: "device_bearer_status", codeKey: "device_bearer_code", status: 403, code: "forbidden"},
		{statusKey: "refresh_status", codeKey: "refresh_code", status: 401, code: "refresh_token_invalid"},
	} {
		if status, err := asInteger(emailGate[expectation.statusKey], "$.x-email-verification-gate."+expectation.statusKey); err != nil || status != expectation.status {
			return fmt.Errorf("$.x-email-verification-gate.%s must be %d", expectation.statusKey, expectation.status)
		}
		if code, _ := emailGate[expectation.codeKey].(string); code != expectation.code {
			return fmt.Errorf("$.x-email-verification-gate.%s must be %q", expectation.codeKey, expectation.code)
		}
	}
	if revoked, ok := emailGate["refresh_family_revoked"].(bool); !ok || !revoked {
		return fmt.Errorf("$.x-email-verification-gate.refresh_family_revoked must be true")
	}
	if newCode, ok := emailGate["new_stable_error_code_required"].(bool); !ok || newCode {
		return fmt.Errorf("$.x-email-verification-gate.new_stable_error_code_required must be false")
	}

	paths, err := requiredObject(doc, "paths", "$")
	if err != nil {
		return err
	}
	lookup, err := contractOperation(paths, "/api/v1/auth/device/authorizations/lookup", "get")
	if err != nil {
		return err
	}
	if err := checkNoStoreHeader(lookup, "200", "GET /api/v1/auth/device/authorizations/lookup"); err != nil {
		return err
	}

	decision, err := contractOperation(paths, "/api/v1/auth/device/authorizations/{id}/decision", "post")
	if err != nil {
		return err
	}
	for _, parameter := range []struct {
		name     string
		required bool
	}{
		{name: "Origin", required: true},
		{name: "X-CSRF-Token", required: true},
		{name: "Sec-Fetch-Site", required: false},
	} {
		header, schema, err := headerParameter(decision, parameter.name, "POST /api/v1/auth/device/authorizations/{id}/decision")
		if err != nil {
			return err
		}
		required, err := asBool(header["required"], "decision parameter "+parameter.name+".required")
		if err != nil || required != parameter.required {
			return fmt.Errorf("decision parameter %s.required must be %t", parameter.name, parameter.required)
		}
		if parameter.name == "X-CSRF-Token" {
			pattern, _ := schema["pattern"].(string)
			if !strings.HasPrefix(pattern, "^csrf_") {
				return fmt.Errorf("decision X-CSRF-Token must use the csrf_ credential pattern")
			}
		}
		if parameter.name == "Sec-Fetch-Site" {
			values, err := schemaEnum(schema)
			if err != nil || !sameStringSet(values, []string{"same-origin"}) {
				return fmt.Errorf("decision Sec-Fetch-Site must allow only same-origin")
			}
		}
	}

	if err := c.checkS2SecuritySchemas(2592000); err != nil {
		return err
	}
	return nil
}

func checkStatusAndError(object map[string]any, wantStatus int64, wantCode, wantAction, label string) error {
	status, err := asInteger(object["status"], label+".status")
	if err != nil || status != wantStatus {
		return fmt.Errorf("%s.status must be %d", label, wantStatus)
	}
	code, _ := object["code"].(string)
	if code != wantCode {
		return fmt.Errorf("%s.code must be %q", label, wantCode)
	}
	if wantAction != "" {
		action, _ := object["action"].(string)
		if action != wantAction {
			return fmt.Errorf("%s.action must be %q", label, wantAction)
		}
	}
	return nil
}

func headerParameter(operation map[string]any, name, label string) (map[string]any, map[string]any, error) {
	parameters, err := asArray(operation["parameters"], label+".parameters")
	if err != nil {
		return nil, nil, err
	}
	for i, raw := range parameters {
		parameter, err := asObject(raw, fmt.Sprintf("%s.parameters[%d]", label, i))
		if err != nil {
			return nil, nil, err
		}
		if parameterName, _ := parameter["name"].(string); parameterName != name {
			continue
		}
		if location, _ := parameter["in"].(string); location != "header" {
			return nil, nil, fmt.Errorf("%s parameter %s must be in header", label, name)
		}
		schema, err := requiredObject(parameter, "schema", label+".parameters."+name)
		if err != nil {
			return nil, nil, err
		}
		return parameter, schema, nil
	}
	return nil, nil, fmt.Errorf("%s must declare header parameter %s", label, name)
}

func (c *checker) checkS2SecuritySchemas(refreshSliding int64) error {
	lookupDoc, err := readJSON(filepath.Join(c.protocolDir, "schemas", "device-authorization-lookup.schema.json"))
	if err != nil {
		return err
	}
	lookup, err := asObject(lookupDoc, "$")
	if err != nil {
		return err
	}
	lookupProperties, err := requiredObject(lookup, "properties", "$")
	if err != nil {
		return err
	}
	csrf, err := requiredObject(lookupProperties, "csrf_token", "$.properties")
	if err != nil {
		return err
	}
	if propertyType, _ := csrf["type"].(string); propertyType != "string" {
		return fmt.Errorf("$.properties.csrf_token.type must be string")
	}
	if pattern, _ := csrf["pattern"].(string); pattern != "^csrf_[A-Za-z0-9_-]+$" {
		return fmt.Errorf("$.properties.csrf_token.pattern must match the csrf_ credential shape")
	}
	allOf, err := asArray(lookup["allOf"], "$.allOf")
	if err != nil {
		return err
	}
	pendingConditionFound := false
	for i, raw := range allOf {
		conditional, err := asObject(raw, fmt.Sprintf("$.allOf[%d]", i))
		if err != nil {
			return err
		}
		ifObject, err := asObject(conditional["if"], fmt.Sprintf("$.allOf[%d].if", i))
		if err != nil {
			continue
		}
		ifProperties, err := requiredObject(ifObject, "properties", fmt.Sprintf("$.allOf[%d].if", i))
		if err != nil {
			continue
		}
		state, err := requiredObject(ifProperties, "state", fmt.Sprintf("$.allOf[%d].if.properties", i))
		if err != nil {
			continue
		}
		if pending, _ := state["const"].(string); pending != "pending" {
			continue
		}
		thenObject, err := requiredObject(conditional, "then", fmt.Sprintf("$.allOf[%d]", i))
		if err != nil {
			return err
		}
		if err := requireStringMember(thenObject, "required", "csrf_token", fmt.Sprintf("$.allOf[%d].then", i)); err != nil {
			return err
		}
		thenProperties, err := requiredObject(thenObject, "properties", fmt.Sprintf("$.allOf[%d].then", i))
		if err != nil {
			return err
		}
		if _, exists := thenProperties["csrf_token"]; !exists {
			return fmt.Errorf("$.allOf[%d].then.properties must define csrf_token", i)
		}
		elseObject, err := requiredObject(conditional, "else", fmt.Sprintf("$.allOf[%d]", i))
		if err != nil {
			return err
		}
		elseProperties, err := requiredObject(elseObject, "properties", fmt.Sprintf("$.allOf[%d].else", i))
		if err != nil {
			return err
		}
		allowed, err := asBool(elseProperties["csrf_token"], fmt.Sprintf("$.allOf[%d].else.properties.csrf_token", i))
		if err != nil || allowed {
			return fmt.Errorf("$.allOf[%d].else.properties.csrf_token must be false", i)
		}
		pendingConditionFound = true
	}
	if !pendingConditionFound {
		return fmt.Errorf("$.allOf must require csrf_token for pending and forbid it otherwise")
	}

	pairDoc, err := readJSON(filepath.Join(c.protocolDir, "schemas", "auth-token-pair.schema.json"))
	if err != nil {
		return err
	}
	pair, err := asObject(pairDoc, "$")
	if err != nil {
		return err
	}
	pairProperties, err := requiredObject(pair, "properties", "$")
	if err != nil {
		return err
	}
	refreshExpires, err := requiredObject(pairProperties, "refresh_expires_in", "$.properties")
	if err != nil {
		return err
	}
	if propertyType, _ := refreshExpires["type"].(string); propertyType != "integer" {
		return fmt.Errorf("$.properties.refresh_expires_in.type must be integer")
	}
	minimum, err := asInteger(refreshExpires["minimum"], "$.properties.refresh_expires_in.minimum")
	if err != nil || minimum != 0 {
		return fmt.Errorf("$.properties.refresh_expires_in.minimum must be 0")
	}
	maximum, err := asInteger(refreshExpires["maximum"], "$.properties.refresh_expires_in.maximum")
	if err != nil || maximum != refreshSliding {
		return fmt.Errorf("$.properties.refresh_expires_in.maximum must be %d", refreshSliding)
	}
	return nil
}

func requireStringMember(object map[string]any, key, want, label string) error {
	values, err := stringArray(object[key], label+"."+key)
	if err != nil {
		return err
	}
	if !containsString(values, want) {
		return fmt.Errorf("%s.%s must contain %q", label, key, want)
	}
	return nil
}

func (c *checker) checkS3Schemas() error {
	errorDoc, err := readJSON(filepath.Join(c.protocolDir, "schemas", "error.schema.json"))
	if err != nil {
		return err
	}
	errorCodes, err := nestedStringEnum(errorDoc, "error", "code")
	if err != nil {
		return fmt.Errorf("error.schema.json: %w", err)
	}
	for _, code := range requiredErrorCodes {
		if !containsString(errorCodes, code) {
			return fmt.Errorf("error.schema.json is missing required S3 code %q", code)
		}
	}

	stateDoc, err := readJSON(filepath.Join(c.protocolDir, "schemas", "playback-ticket-state.schema.json"))
	if err != nil {
		return err
	}
	states, err := schemaEnum(stateDoc)
	if err != nil {
		return fmt.Errorf("playback-ticket-state.schema.json: %w", err)
	}
	if !sameStringSet(states, requiredTicketStates) {
		return fmt.Errorf("playback-ticket-state.schema.json must contain exactly the S3 state set; got %v", states)
	}

	mediaDoc, err := readJSON(filepath.Join(c.protocolDir, "schemas", "media.schema.json"))
	if err != nil {
		return err
	}
	mediaObject, err := asObject(mediaDoc, "$")
	if err != nil {
		return err
	}
	properties, err := requiredObject(mediaObject, "properties", "$")
	if err != nil {
		return err
	}
	audio, err := requiredObject(properties, "audio", "$.properties")
	if err != nil {
		return err
	}
	minItems, err := asInteger(audio["minItems"], "$.properties.audio.minItems")
	if err != nil || minItems != 0 {
		return fmt.Errorf("media.schema.json audio.minItems must be 0")
	}

	transportDoc, err := readJSON(filepath.Join(c.protocolDir, "schemas", "transport.schema.json"))
	if err != nil {
		return err
	}
	transportObject, err := asObject(transportDoc, "$")
	if err != nil {
		return err
	}
	oneOf, err := asArray(transportObject["oneOf"], "$.oneOf")
	if err != nil {
		return err
	}
	var transportRefs []string
	for i, raw := range oneOf {
		option, err := asObject(raw, fmt.Sprintf("$.oneOf[%d]", i))
		if err != nil {
			return err
		}
		ref, err := asString(option["$ref"], fmt.Sprintf("$.oneOf[%d].$ref", i))
		if err != nil {
			return err
		}
		transportRefs = append(transportRefs, ref)
	}
	wantTransportRefs := []string{
		"transport-direct-http.schema.json",
		"transport-server-proxy.schema.json",
		"transport-server-hls.schema.json",
		"transport-direct-dash.schema.json",
	}
	if !sameStringSet(transportRefs, wantTransportRefs) || len(transportRefs) != len(wantTransportRefs) {
		return fmt.Errorf("transport.schema.json must contain exactly the four v1 transports, got %v", transportRefs)
	}

	statusDoc, err := readJSON(filepath.Join(c.protocolDir, "schemas", "playback-ticket.schema.json"))
	if err != nil {
		return err
	}
	statusObject, err := asObject(statusDoc, "$")
	if err != nil {
		return err
	}
	statusProperties, err := requiredObject(statusObject, "properties", "$")
	if err != nil {
		return err
	}
	for _, forbidden := range []string{
		"transport", "streams", "url", "media_session", "headers",
		"subtitles", "capability", "capabilities",
	} {
		if _, exists := statusProperties[forbidden]; exists {
			return fmt.Errorf("playback-ticket.schema.json GET projection must not expose property %q", forbidden)
		}
	}

	issuanceDoc, err := readJSON(filepath.Join(c.protocolDir, "schemas", "playback-ticket-issuance.schema.json"))
	if err != nil {
		return err
	}
	issuanceObject, err := asObject(issuanceDoc, "$")
	if err != nil {
		return err
	}
	issuanceProperties, err := requiredObject(issuanceObject, "properties", "$")
	if err != nil {
		return err
	}
	if _, exists := issuanceProperties["transport"]; !exists {
		return fmt.Errorf("playback-ticket-issuance.schema.json must expose transport for consuming responses")
	}

	subtitleDoc, err := readJSON(filepath.Join(c.protocolDir, "schemas", "subtitle.schema.json"))
	if err != nil {
		return err
	}
	subtitleObject, err := asObject(subtitleDoc, "$")
	if err != nil {
		return err
	}
	subtitleProperties, err := requiredObject(subtitleObject, "properties", "$")
	if err != nil {
		return err
	}
	for _, required := range []string{
		"id", "language", "label", "format", "default", "forced",
	} {
		if _, exists := subtitleProperties[required]; !exists {
			return fmt.Errorf("subtitle.schema.json is missing property %q", required)
		}
	}
	if _, exists := subtitleProperties["url"]; !exists {
		return fmt.Errorf("subtitle.schema.json must expose an optional url property")
	}
	formatObject, err := requiredObject(subtitleProperties, "format", "$.properties")
	if err != nil {
		return err
	}
	formats, err := schemaEnum(formatObject)
	if err != nil {
		return fmt.Errorf("subtitle.schema.json format: %w", err)
	}
	if !sameStringSet(formats, []string{"vtt", "srt", "ass", "pgs"}) {
		return fmt.Errorf("subtitle.schema.json format enum = %v", formats)
	}

	for _, name := range []string{
		"transport-direct-http.schema.json",
		"transport-server-proxy.schema.json",
		"transport-server-hls.schema.json",
		"transport-direct-dash.schema.json",
	} {
		doc, err := readJSON(filepath.Join(c.protocolDir, "schemas", name))
		if err != nil {
			return err
		}
		object, err := asObject(doc, "$")
		if err != nil {
			return err
		}
		properties, err := requiredObject(object, "properties", "$")
		if err != nil {
			return err
		}
		if _, exists := properties["subtitles"]; !exists {
			return fmt.Errorf("%s must expose subtitles", name)
		}
	}

	hlsDoc, err := readJSON(filepath.Join(
		c.protocolDir, "schemas", "transport-server-hls.schema.json"))
	if err != nil {
		return err
	}
	hlsObject, err := asObject(hlsDoc, "$")
	if err != nil {
		return err
	}
	hlsProperties, err := requiredObject(hlsObject, "properties", "$")
	if err != nil {
		return err
	}
	hlsSubtitles, err := requiredObject(hlsProperties, "subtitles", "$.properties")
	if err != nil {
		return err
	}
	hlsItems, err := requiredObject(hlsSubtitles, "items", "$.properties.subtitles")
	if err != nil {
		return err
	}
	allOf, err := asArray(hlsItems["allOf"], "$.properties.subtitles.items.allOf")
	if err != nil || len(allOf) < 2 {
		return fmt.Errorf("server-HLS subtitle items must require VTT and a nested ss_ URL")
	}
	hlsConstraint, err := asObject(allOf[1], "$.properties.subtitles.items.allOf[1]")
	if err != nil {
		return err
	}
	hlsConstraints, err := requiredObject(
		hlsConstraint, "properties", "$.properties.subtitles.items.allOf[1]")
	if err != nil {
		return err
	}
	hlsFormat, err := requiredObject(
		hlsConstraints, "format", "$.properties.subtitles.items.allOf[1].properties")
	if err != nil {
		return err
	}
	if hlsFormat["const"] != "vtt" {
		return fmt.Errorf("server-HLS subtitles must be WebVTT only")
	}
	hlsURL, err := requiredObject(
		hlsConstraints, "url", "$.properties.subtitles.items.allOf[1].properties")
	if err != nil {
		return err
	}
	hlsPattern, _ := hlsURL["pattern"].(string)
	if !strings.Contains(hlsPattern, "media-sessions/ms_") ||
		!strings.Contains(hlsPattern, "/subtitle-sessions/ss_") {
		return fmt.Errorf("server-HLS subtitle URLs must bind ms_ and ss_ capabilities")
	}

	for _, name := range []string{"playback-ticket-create-request.schema.json", "playback-client-capabilities.schema.json"} {
		doc, err := readJSON(filepath.Join(c.protocolDir, "schemas", name))
		if err != nil {
			return err
		}
		if err := checkForbiddenRequestProperties(doc, name); err != nil {
			return err
		}
	}

	sessionDoc, err := readJSON(filepath.Join(c.protocolDir, "schemas", "media-session.schema.json"))
	if err != nil {
		return err
	}
	sessionObject, err := asObject(sessionDoc, "$")
	if err != nil {
		return err
	}
	sessionProperties, err := requiredObject(sessionObject, "properties", "$")
	if err != nil {
		return err
	}
	if _, exists := sessionProperties["session_id"]; exists {
		return fmt.Errorf("media-session.schema.json must not expose a separate session_id capability field")
	}
	return nil
}

func checkPlaybackMediaContract(value any) error {
	root, err := asObject(value, "$")
	if err != nil {
		return err
	}
	paths, err := requiredObject(root, "paths", "$")
	if err != nil {
		return err
	}
	components, err := requiredObject(root, "components", "$")
	if err != nil {
		return err
	}
	responses, err := requiredObject(components, "responses", "$.components")
	if err != nil {
		return err
	}

	streamPath := "/api/v1/playback/media-sessions/{id}/stream"
	streamStatuses := []string{
		"200", "206", "400", "403", "404", "408", "410", "416",
		"429", "500", "502", "504",
	}
	for _, method := range []string{"get", "head"} {
		operation, err := playbackOperation(paths, streamPath, method)
		if err != nil {
			return err
		}
		label := strings.ToUpper(method) + " " + streamPath
		if err := checkParameterRefs(operation, label,
			"#/components/parameters/MediaSessionId",
			"#/components/parameters/Range",
			"#/components/parameters/IfRange",
		); err != nil {
			return err
		}
		operationResponses, err := asObject(operation["responses"], label+".responses")
		if err != nil {
			return err
		}
		for _, status := range streamStatuses {
			if _, ok := operationResponses[status]; !ok {
				return fmt.Errorf("%s must declare %s", label, status)
			}
		}
		if method == "head" {
			for status, raw := range operationResponses {
				response, err := asObject(raw, label+".responses["+status+"]")
				if err != nil {
					return err
				}
				if _, exists := response["content"]; exists {
					return fmt.Errorf("%s response %s must not declare a response body", label, status)
				}
				if _, exists := response["$ref"]; exists {
					return fmt.Errorf("%s response %s must be inline and bodyless", label, status)
				}
			}
			continue
		}
		for _, status := range []string{"200", "206"} {
			wantRef := "#/components/responses/ServerProxyStream"
			if status == "206" {
				wantRef = "#/components/responses/ServerProxyPartialContent"
			}
			if err := checkInlineResponseRef(
				operationResponses, status, label, wantRef); err != nil {
				return err
			}
		}
		for _, status := range []string{
			"400", "403", "404", "408", "410", "429", "500", "502", "504",
		} {
			if err := checkInlineResponseRef(
				operationResponses, status, label,
				"#/components/responses/StableError"); err != nil {
				return err
			}
		}
	}

	hlsStatuses := []string{
		"200", "400", "403", "404", "408", "410", "415", "500", "502", "504",
	}
	for _, tc := range []struct {
		path     string
		response string
	}{
		{
			path:     "/api/v1/playback/media-sessions/{id}/index.m3u8",
			response: "ServerHLSPlaylist",
		},
		{
			path:     "/api/v1/playback/media-sessions/{id}/{segment}",
			response: "ServerHLSSegment",
		},
	} {
		operation, err := playbackOperation(paths, tc.path, "get")
		if err != nil {
			return err
		}
		label := "GET " + tc.path
		operationResponses, err := asObject(operation["responses"], label+".responses")
		if err != nil {
			return err
		}
		for _, status := range hlsStatuses {
			if _, ok := operationResponses[status]; !ok {
				return fmt.Errorf("%s must declare %s", label, status)
			}
		}
		if err := checkInlineResponseRef(
			operationResponses, "200", label,
			"#/components/responses/"+tc.response); err != nil {
			return err
		}
		for _, status := range []string{
			"400", "403", "404", "408", "410", "415", "500", "502", "504",
		} {
			if err := checkInlineResponseRef(
				operationResponses, status, label,
				"#/components/responses/StableError"); err != nil {
				return err
			}
		}
	}

	hlsContract, err := requiredObject(
		root, "x-server-hls-media-session", "$")
	if err != nil {
		return err
	}
	stableErrors, err := requiredObject(
		hlsContract, "stable_errors", "$.x-server-hls-media-session")
	if err != nil {
		return err
	}
	for status, wantCodes := range map[string][]string{
		"400": {"invalid_request"},
		"408": {"invalid_request"},
		"502": {"source_unavailable"},
		"504": {"internal_error"},
	} {
		raw, ok := stableErrors[status]
		if !ok {
			return fmt.Errorf("$.x-server-hls-media-session.stable_errors must declare %s", status)
		}
		codes, err := stringArray(raw, "$.x-server-hls-media-session.stable_errors["+status+"]")
		if err != nil {
			return err
		}
		if !sameStringSet(codes, wantCodes) {
			return fmt.Errorf("$.x-server-hls-media-session.stable_errors[%s] = %v, want %v",
				status, codes, wantCodes)
		}
	}

	if _, err := requiredObject(responses, "StableError", "$.components.responses"); err != nil {
		return err
	}
	return nil
}

func checkParameterRefs(operation map[string]any, label string, want ...string) error {
	parameters, err := asArray(operation["parameters"], label+".parameters")
	if err != nil {
		return err
	}
	if len(parameters) != len(want) {
		return fmt.Errorf("%s must declare exactly %d parameters", label, len(want))
	}
	refs := make([]string, 0, len(parameters))
	for i, raw := range parameters {
		parameter, err := asObject(raw, fmt.Sprintf("%s.parameters[%d]", label, i))
		if err != nil {
			return err
		}
		ref, err := asString(parameter["$ref"], fmt.Sprintf("%s.parameters[%d].$ref", label, i))
		if err != nil {
			return err
		}
		refs = append(refs, ref)
	}
	if !sameStringSet(refs, want) {
		return fmt.Errorf("%s parameter refs = %v, want %v", label, refs, want)
	}
	return nil
}

func checkInlineResponseRef(
	responses map[string]any,
	status, label, wantRef string,
) error {
	response, err := requiredObject(responses, status, label+".responses")
	if err != nil {
		return err
	}
	if ref, _ := response["$ref"].(string); ref != wantRef {
		return fmt.Errorf("%s response %s must reference %s, got %q",
			label, status, wantRef, ref)
	}
	return nil
}

func checkPlaybackRangeContract(value any) error {
	root, err := asObject(value, "$")
	if err != nil {
		return err
	}
	contract, err := requiredObject(root, "x-playback-byte-ranges", "$")
	if err != nil {
		return err
	}
	if unit, _ := contract["unit"].(string); unit != "bytes" {
		return fmt.Errorf("$.x-playback-byte-ranges.unit must be bytes")
	}
	request, err := requiredObject(
		contract, "request", "$.x-playback-byte-ranges")
	if err != nil {
		return err
	}
	if err := checkStringSetField(request, "outer_ows",
		[]string{"SP", "HTAB"},
		"$.x-playback-byte-ranges.request"); err != nil {
		return err
	}
	if err := checkStringSetField(request, "accepted_forms",
		[]string{"closed", "open-ended"},
		"$.x-playback-byte-ranges.request"); err != nil {
		return err
	}
	if err := checkStringSetField(request, "rejected_forms",
		[]string{"suffix", "multi-range", "repeated-header", "reversed"},
		"$.x-playback-byte-ranges.request"); err != nil {
		return err
	}
	for key, want := range map[string]int64{
		"malformed_status":     http.StatusBadRequest,
		"unsatisfiable_status": http.StatusRequestedRangeNotSatisfiable,
	} {
		got, err := asInteger(request[key], "$.x-playback-byte-ranges.request."+key)
		if err != nil || got != want {
			return fmt.Errorf("$.x-playback-byte-ranges.request.%s must be %d", key, want)
		}
	}
	if code, _ := request["malformed_code"].(string); code != "invalid_request" {
		return fmt.Errorf("$.x-playback-byte-ranges.request.malformed_code must be invalid_request")
	}
	if pattern, _ := request["unsatisfiable_content_range_pattern"].(string); pattern != `^bytes \*/[0-9]+$` {
		return fmt.Errorf("$.x-playback-byte-ranges.request.unsatisfiable_content_range_pattern is invalid")
	}

	response, err := requiredObject(
		contract, "response", "$.x-playback-byte-ranges")
	if err != nil {
		return err
	}
	partial, err := asInteger(
		response["partial_status"], "$.x-playback-byte-ranges.response.partial_status")
	if err != nil || partial != http.StatusPartialContent {
		return fmt.Errorf("$.x-playback-byte-ranges.response.partial_status must be 206")
	}
	required, ok := response["content_range_required"].(bool)
	if !ok || !required {
		return fmt.Errorf("$.x-playback-byte-ranges.response.content_range_required must be true")
	}
	if pattern, _ := response["content_range_pattern"].(string); pattern != `^bytes [0-9]+-[0-9]+/[0-9]+$` {
		return fmt.Errorf("$.x-playback-byte-ranges.response.content_range_pattern is invalid")
	}
	bodyless, ok := response["head_bodyless"].(bool)
	if !ok || !bodyless {
		return fmt.Errorf("$.x-playback-byte-ranges.response.head_bodyless must be true")
	}

	validators, err := requiredObject(
		contract, "validators", "$.x-playback-byte-ranges")
	if err != nil {
		return err
	}
	if localETag, _ := validators["local_etag"].(string); localETag != "strong" {
		return fmt.Errorf("$.x-playback-byte-ranges.validators.local_etag must be strong")
	}
	ifRange, err := requiredObject(
		validators, "if_range", "$.x-playback-byte-ranges.validators")
	if err != nil {
		return err
	}
	if accepted, _ := ifRange["accepted"].(string); accepted != "strong-etag-or-http-date" {
		return fmt.Errorf("$.x-playback-byte-ranges.validators.if_range.accepted must be strong-etag-or-http-date")
	}
	for key, want := range map[string]int64{
		"local_date_weak_stale_or_unavailable_status": http.StatusOK,
		"conflicting_partial_status":                  http.StatusBadGateway,
	} {
		got, err := asInteger(ifRange[key],
			"$.x-playback-byte-ranges.validators.if_range."+key)
		if err != nil || got != want {
			return fmt.Errorf("$.x-playback-byte-ranges.validators.if_range.%s must be %d", key, want)
		}
	}
	forwards, ok := ifRange["server_proxy_forwards_condition"].(bool)
	if !ok || !forwards {
		return fmt.Errorf("$.x-playback-byte-ranges.validators.if_range.server_proxy_forwards_condition must be true")
	}

	components, err := requiredObject(root, "components", "$")
	if err != nil {
		return err
	}
	parameters, err := requiredObject(components, "parameters", "$.components")
	if err != nil {
		return err
	}
	rangeParameter, err := requiredObject(parameters, "Range", "$.components.parameters")
	if err != nil {
		return err
	}
	rangeSchema, err := requiredObject(
		rangeParameter, "schema", "$.components.parameters.Range")
	if err != nil {
		return err
	}
	if schemaType, _ := rangeSchema["type"].(string); schemaType != "string" {
		return fmt.Errorf("Range.schema.type must be string")
	}
	wantRangePattern := `^[ \t]*[bB][yY][tT][eE][sS]=[0-9]+-(?:[0-9]+)?[ \t]*$`
	if pattern, _ := rangeSchema["pattern"].(string); pattern != wantRangePattern {
		return fmt.Errorf("Range.schema.pattern must encode one closed/open range with outer SP/HTAB OWS")
	}
	if single, ok := rangeParameter["x-single-range"].(bool); !ok || !single {
		return fmt.Errorf("Range.x-single-range must be true")
	}
	if status, err := asInteger(rangeParameter["x-malformed-status"], "Range.x-malformed-status"); err != nil || status != http.StatusBadRequest {
		return fmt.Errorf("Range.x-malformed-status must be 400")
	}
	for _, key := range []string{"x-outer-ows", "x-accepted-forms", "x-rejected-forms"} {
		if _, ok := rangeParameter[key]; !ok {
			return fmt.Errorf("Range.%s is required", key)
		}
	}
	if err := checkStringSetField(rangeParameter, "x-outer-ows",
		[]string{"SP", "HTAB"}, "$.components.parameters.Range"); err != nil {
		return err
	}
	if err := checkStringSetField(rangeParameter, "x-accepted-forms",
		[]string{"closed", "open-ended"}, "$.components.parameters.Range"); err != nil {
		return err
	}
	if err := checkStringSetField(rangeParameter, "x-rejected-forms",
		[]string{"suffix", "multi-range", "repeated-header", "reversed"},
		"$.components.parameters.Range"); err != nil {
		return err
	}

	ifRangeParameter, err := requiredObject(
		parameters, "IfRange", "$.components.parameters")
	if err != nil {
		return err
	}
	if accepted, _ := ifRangeParameter["x-if-range-accepted"].(string); accepted != "strong-etag-or-http-date" {
		return fmt.Errorf("IfRange.x-if-range-accepted must be strong-etag-or-http-date")
	}
	if _, exists := ifRangeParameter["x-if-range-accepted-http-date"]; exists {
		return fmt.Errorf("IfRange.x-if-range-accepted-http-date is obsolete; x-if-range-accepted includes HTTP-date")
	}
	if exactStrong, ok := ifRangeParameter["x-local-exact-strong-etag-required"].(bool); !ok || !exactStrong {
		return fmt.Errorf("IfRange.x-local-exact-strong-etag-required must be true")
	}
	if forwarded, _ := ifRangeParameter["x-if-range-forwarded-by-server-proxy"].(string); forwarded != "single-value-raw" {
		return fmt.Errorf("IfRange.x-if-range-forwarded-by-server-proxy must be single-value-raw")
	}
	if ignored, ok := ifRangeParameter["x-if-range-ignored-without-range"].(bool); !ok || !ignored {
		return fmt.Errorf("IfRange.x-if-range-ignored-without-range must be true")
	}
	if status, err := asInteger(ifRangeParameter["x-if-range-repeated-status"], "IfRange.x-if-range-repeated-status"); err != nil || status != http.StatusBadRequest {
		return fmt.Errorf("IfRange.x-if-range-repeated-status must be 400")
	}
	if status, err := asInteger(ifRangeParameter["x-local-nonmatch-status"], "IfRange.x-local-nonmatch-status"); err != nil || status != http.StatusOK {
		return fmt.Errorf("IfRange.x-local-nonmatch-status must be 200")
	}
	if status, err := asInteger(ifRangeParameter["x-server-proxy-conflict-status"], "IfRange.x-server-proxy-conflict-status"); err != nil || status != http.StatusBadGateway {
		return fmt.Errorf("IfRange.x-server-proxy-conflict-status must be 502")
	}
	ifRangeSchema, err := requiredObject(
		ifRangeParameter, "schema", "$.components.parameters.IfRange")
	if err != nil {
		return err
	}
	if len(ifRangeSchema) != 1 {
		return fmt.Errorf("IfRange.schema must only declare an unconstrained string header value")
	}
	if schemaType, _ := ifRangeSchema["type"].(string); schemaType != "string" {
		return fmt.Errorf("IfRange.schema.type must be string")
	}

	componentResponses, err := requiredObject(
		components, "responses", "$.components")
	if err != nil {
		return err
	}
	for _, name := range []string{"ServerProxyStream", "ServerProxyPartialContent"} {
		component, err := requiredObject(componentResponses, name, "$.components.responses")
		if err != nil {
			return err
		}
		headers, err := requiredObject(component, "headers", "$.components.responses."+name)
		if err != nil {
			return err
		}
		etag, err := requiredObject(headers, "ETag", "$.components.responses."+name+".headers")
		if err != nil {
			return err
		}
		if value, _ := etag["x-if-range-validator"].(string); value != "strong-exact-only" {
			return fmt.Errorf("%s ETag must require a strong exact If-Range validator", name)
		}
		etagSchema, err := requiredObject(
			etag, "schema", "$.components.responses."+name+".headers.ETag")
		if err != nil {
			return err
		}
		if schemaType, _ := etagSchema["type"].(string); schemaType != "string" {
			return fmt.Errorf("%s ETag.schema.type must be string", name)
		}
	}
	partialResponse, err := requiredObject(
		componentResponses, "ServerProxyPartialContent", "$.components.responses")
	if err != nil {
		return err
	}
	partialHeaders, err := requiredObject(
		partialResponse, "headers", "$.components.responses.ServerProxyPartialContent")
	if err != nil {
		return err
	}
	contentRange, err := requiredObject(
		partialHeaders, "Content-Range",
		"$.components.responses.ServerProxyPartialContent.headers")
	if err != nil {
		return err
	}
	if status, err := asInteger(
		contentRange["x-required-on-status"],
		"ServerProxyPartialContent.headers.Content-Range.x-required-on-status"); err != nil || status != http.StatusPartialContent {
		return fmt.Errorf("206 Content-Range must be required")
	}
	contentRangeSchema, err := requiredObject(
		contentRange, "schema",
		"$.components.responses.ServerProxyPartialContent.headers.Content-Range")
	if err != nil {
		return err
	}
	if pattern, _ := contentRangeSchema["pattern"].(string); pattern != `^bytes [0-9]+-[0-9]+/[0-9]+$` {
		return fmt.Errorf("206 Content-Range pattern is invalid")
	}

	paths, err := requiredObject(root, "paths", "$")
	if err != nil {
		return err
	}
	head, err := playbackOperation(
		paths, "/api/v1/playback/media-sessions/{id}/stream", "head")
	if err != nil {
		return err
	}
	headResponses, err := asObject(
		head["responses"], "HEAD media responses")
	if err != nil {
		return err
	}
	if err := checkResponseHeader(
		headResponses, "206", "Content-Range",
		http.StatusPartialContent, `^bytes [0-9]+-[0-9]+/[0-9]+$`,
		"HEAD media"); err != nil {
		return err
	}
	if err := checkResponseHeader(
		headResponses, "416", "Content-Range",
		http.StatusRequestedRangeNotSatisfiable, `^bytes \*/[0-9]+$`,
		"HEAD media"); err != nil {
		return err
	}
	for _, status := range []string{"200", "206"} {
		response, err := requiredObject(headResponses, status, "HEAD media.responses")
		if err != nil {
			return err
		}
		headers, err := requiredObject(
			response, "headers", "HEAD media.responses["+status+"]")
		if err != nil {
			return err
		}
		etag, err := requiredObject(
			headers, "ETag", "HEAD media.responses["+status+"].headers")
		if err != nil {
			return err
		}
		if value, _ := etag["x-if-range-validator"].(string); value != "strong-exact-only" {
			return fmt.Errorf("HEAD media %s ETag must require a strong exact If-Range validator", status)
		}
	}
	return nil
}

func checkResponseHeader(
	responses map[string]any,
	status, name string,
	requiredStatus int64,
	pattern, label string,
) error {
	response, err := requiredObject(responses, status, label+".responses")
	if err != nil {
		return err
	}
	headers, err := requiredObject(
		response, "headers", label+".responses["+status+"]")
	if err != nil {
		return err
	}
	header, err := requiredObject(
		headers, name, label+".responses["+status+"].headers")
	if err != nil {
		return err
	}
	required, err := asInteger(
		header["x-required-on-status"],
		label+".responses["+status+"].headers."+name+".x-required-on-status")
	if err != nil || required != requiredStatus {
		return fmt.Errorf("%s %s %s must be required on %d",
			label, status, name, requiredStatus)
	}
	schema, err := requiredObject(
		header, "schema", label+".responses["+status+"].headers."+name)
	if err != nil {
		return err
	}
	if got, _ := schema["pattern"].(string); got != pattern {
		return fmt.Errorf("%s %s %s pattern = %q, want %q",
			label, status, name, got, pattern)
	}
	return nil
}

func checkStringSetField(
	parent map[string]any,
	key string,
	want []string,
	path string,
) error {
	raw, ok := parent[key]
	if !ok {
		return fmt.Errorf("%s.%s is required", path, key)
	}
	got, err := stringArray(raw, path+"."+key)
	if err != nil {
		return err
	}
	if !sameStringSet(got, want) {
		return fmt.Errorf("%s.%s = %v, want %v", path, key, got, want)
	}
	return nil
}

func (c *checker) checkPlaybackFailureCodeContract(openapiDoc any) error {
	failureCodeDoc, err := readJSON(filepath.Join(
		c.protocolDir, "schemas", "playback-failure-code.schema.json"))
	if err != nil {
		return err
	}
	statusDoc, err := readJSON(filepath.Join(
		c.protocolDir, "schemas", "playback-ticket.schema.json"))
	if err != nil {
		return err
	}
	issuanceDoc, err := readJSON(filepath.Join(
		c.protocolDir, "schemas", "playback-ticket-issuance.schema.json"))
	if err != nil {
		return err
	}
	return checkPlaybackFailureCodeDocs(
		openapiDoc, failureCodeDoc, statusDoc, issuanceDoc)
}

func checkPlaybackFailureCodeDocs(
	openapiDoc, failureCodeDoc, statusDoc, issuanceDoc any,
) error {
	failedCodes := []string{
		"source_unavailable",
		"probe_timeout",
		"probe_interrupted",
		"probe_finalize_failed",
	}
	unsupportedCodes := []string{"unsupported_media"}
	allCodes := append(append([]string(nil), failedCodes...), unsupportedCodes...)

	root, err := asObject(openapiDoc, "$")
	if err != nil {
		return err
	}
	contract, err := requiredObject(root, "x-playback-failure-codes", "$")
	if err != nil {
		return err
	}
	if err := checkStringSetField(
		contract, "failed", failedCodes,
		"$.x-playback-failure-codes"); err != nil {
		return err
	}
	if err := checkStringSetField(
		contract, "unsupported", unsupportedCodes,
		"$.x-playback-failure-codes"); err != nil {
		return err
	}
	if fallback, _ := contract["unknown_internal_failed_fallback"].(string); fallback != "source_unavailable" {
		return fmt.Errorf("$.x-playback-failure-codes.unknown_internal_failed_fallback must be source_unavailable")
	}

	codes, err := schemaEnum(failureCodeDoc)
	if err != nil {
		return fmt.Errorf("playback-failure-code.schema.json: %w", err)
	}
	if !sameStringSet(codes, allCodes) {
		return fmt.Errorf("playback-failure-code.schema.json enum = %v, want %v", codes, allCodes)
	}

	for name, doc := range map[string]any{
		"playback-ticket.schema.json":          statusDoc,
		"playback-ticket-issuance.schema.json": issuanceDoc,
	} {
		object, err := asObject(doc, "$")
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		properties, err := requiredObject(object, "properties", "$")
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		failureProperty, err := requiredObject(properties, "failure_code", "$.properties")
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if ref, _ := failureProperty["$ref"].(string); ref != "playback-failure-code.schema.json" {
			return fmt.Errorf("%s failure_code must reference playback-failure-code.schema.json", name)
		}
		unsupported, err := failureCodeConstraint(object, "unsupported")
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if value, _ := unsupported["const"].(string); value != "unsupported_media" {
			return fmt.Errorf("%s unsupported failure_code must be unsupported_media", name)
		}
		failed, err := failureCodeConstraint(object, "failed")
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		failedEnum, err := schemaEnum(failed)
		if err != nil {
			return fmt.Errorf("%s failed failure_code: %w", name, err)
		}
		if !sameStringSet(failedEnum, failedCodes) {
			return fmt.Errorf("%s failed failure_code enum = %v, want %v",
				name, failedEnum, failedCodes)
		}
		forbidden, forbiddenStates, err :=
			failureCodeForbiddenConstraint(object)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if !sameStringSet(forbiddenStates, []string{"failed", "unsupported"}) {
			return fmt.Errorf("%s failure_code must be forbidden for every other state, got allowed states %v",
				name, forbiddenStates)
		}
		if value, ok := forbidden["failure_code"].(bool); !ok || value {
			return fmt.Errorf("%s non-terminal failure_code must be false", name)
		}
	}
	return nil
}

func failureCodeConstraint(doc map[string]any, state string) (map[string]any, error) {
	allOf, err := asArray(doc["allOf"], "$.allOf")
	if err != nil {
		return nil, err
	}
	for i, raw := range allOf {
		item, err := asObject(raw, fmt.Sprintf("$.allOf[%d]", i))
		if err != nil {
			return nil, err
		}
		condition, err := asObject(item["if"], fmt.Sprintf("$.allOf[%d].if", i))
		if err != nil {
			continue
		}
		conditionProperties, err := asObject(
			condition["properties"], fmt.Sprintf("$.allOf[%d].if.properties", i))
		if err != nil {
			continue
		}
		stateCondition, err := asObject(
			conditionProperties["state"],
			fmt.Sprintf("$.allOf[%d].if.properties.state", i))
		if err != nil {
			continue
		}
		constant, _ := stateCondition["const"].(string)
		if constant != state {
			continue
		}
		then, err := asObject(item["then"], fmt.Sprintf("$.allOf[%d].then", i))
		if err != nil {
			return nil, err
		}
		thenProperties, err := asObject(
			then["properties"], fmt.Sprintf("$.allOf[%d].then.properties", i))
		if err != nil {
			return nil, err
		}
		constraint, err := asObject(
			thenProperties["failure_code"],
			fmt.Sprintf("$.allOf[%d].then.properties.failure_code", i))
		if err != nil {
			return nil, err
		}
		return constraint, nil
	}
	return nil, fmt.Errorf("missing failure_code constraint for state %q", state)
}

func failureCodeForbiddenConstraint(
	doc map[string]any,
) (map[string]any, []string, error) {
	allOf, err := asArray(doc["allOf"], "$.allOf")
	if err != nil {
		return nil, nil, err
	}
	for i, raw := range allOf {
		item, err := asObject(raw, fmt.Sprintf("$.allOf[%d]", i))
		if err != nil {
			return nil, nil, err
		}
		condition, err := asObject(item["if"], fmt.Sprintf("$.allOf[%d].if", i))
		if err != nil {
			continue
		}
		conditionProperties, err := asObject(
			condition["properties"], fmt.Sprintf("$.allOf[%d].if.properties", i))
		if err != nil {
			continue
		}
		stateCondition, err := asObject(
			conditionProperties["state"],
			fmt.Sprintf("$.allOf[%d].if.properties.state", i))
		if err != nil {
			continue
		}
		notCondition, err := asObject(
			stateCondition["not"],
			fmt.Sprintf("$.allOf[%d].if.properties.state.not", i))
		if err != nil {
			continue
		}
		states, err := schemaEnum(notCondition)
		if err != nil {
			continue
		}
		then, err := asObject(item["then"], fmt.Sprintf("$.allOf[%d].then", i))
		if err != nil {
			return nil, nil, err
		}
		thenProperties, err := asObject(
			then["properties"], fmt.Sprintf("$.allOf[%d].then.properties", i))
		if err != nil {
			return nil, nil, err
		}
		if _, ok := thenProperties["failure_code"]; !ok {
			return nil, nil, fmt.Errorf(
				"$.allOf[%d].then.properties.failure_code is required", i)
		}
		return thenProperties, states, nil
	}
	return nil, nil, errors.New(
		"missing condition forbidding failure_code outside failed/unsupported")
}

func nestedStringEnum(value any, objectKey, propertyKey string) ([]string, error) {
	doc, err := asObject(value, "$")
	if err != nil {
		return nil, err
	}
	rootProperties, err := requiredObject(doc, "properties", "$")
	if err != nil {
		return nil, err
	}
	object, err := requiredObject(rootProperties, objectKey, "$.properties")
	if err != nil {
		return nil, err
	}
	properties, err := requiredObject(object, "properties", "$.properties."+objectKey)
	if err != nil {
		return nil, err
	}
	property, err := requiredObject(properties, propertyKey, "$."+objectKey+".properties")
	if err != nil {
		return nil, err
	}
	return schemaEnum(property)
}

func schemaEnum(value any) ([]string, error) {
	doc, err := asObject(value, "$")
	if err != nil {
		return nil, err
	}
	raw, err := asArray(doc["enum"], "$.enum")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(raw))
	for i, item := range raw {
		text, err := asString(item, fmt.Sprintf("$.enum[%d]", i))
		if err != nil {
			return nil, err
		}
		out = append(out, text)
	}
	return out, nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy := append([]string(nil), left...)
	rightCopy := append([]string(nil), right...)
	sort.Strings(leftCopy)
	sort.Strings(rightCopy)
	for i := range leftCopy {
		if leftCopy[i] != rightCopy[i] {
			return false
		}
	}
	return true
}

func checkForbiddenRequestProperties(value any, label string) error {
	forbidden := make(map[string]bool, len(forbiddenPlaybackRequestKeys))
	for _, key := range forbiddenPlaybackRequestKeys {
		forbidden[strings.ToLower(strings.ReplaceAll(key, "-", "_"))] = true
	}
	var problems []string
	var walk func(current any, path string)
	walk = func(current any, path string) {
		switch typed := current.(type) {
		case map[string]any:
			if rawProperties, ok := typed["properties"].(map[string]any); ok {
				for property := range rawProperties {
					normalized := strings.ToLower(strings.ReplaceAll(property, "-", "_"))
					if forbidden[normalized] {
						problems = append(problems, fmt.Sprintf("%s%s must not expose request property %q", label, path, property))
					}
				}
			}
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				walk(typed[key], path+"."+key)
			}
		case []any:
			for i, item := range typed {
				walk(item, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(value, "$")
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

func (c *checker) checkRequiredFiles() error {
	var missing []string
	required := []string{
		filepath.Join(c.protocolDir, "openapi.yaml"),
		filepath.Join(c.protocolDir, "README.md"),
	}
	for _, name := range requiredSchemas {
		required = append(required, filepath.Join(c.protocolDir, "schemas", name))
	}
	for _, name := range requiredFixtures {
		required = append(required, filepath.Join(c.protocolDir, "fixtures", name))
	}
	for _, path := range required {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			missing = append(missing, c.rel(path))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required files:\n  %s", strings.Join(missing, "\n  "))
	}
	return nil
}

func (c *checker) checkOpenAPIShape(value any) error {
	doc, err := asObject(value, "$")
	if err != nil {
		return err
	}

	version, err := asString(doc["openapi"], "$.openapi")
	if err != nil {
		return err
	}
	if !strings.HasPrefix(version, "3.1.") {
		return fmt.Errorf("$.openapi must be a 3.1.x version, got %q", version)
	}
	contractVersion, err := asInteger(doc["x-contract-version"], "$.x-contract-version")
	if err != nil {
		return err
	}
	if contractVersion != 1 {
		return fmt.Errorf("$.x-contract-version must be 1")
	}

	info, err := asObject(doc["info"], "$.info")
	if err != nil {
		return err
	}
	if _, err := asString(info["title"], "$.info.title"); err != nil {
		return err
	}
	if _, err := asString(info["version"], "$.info.version"); err != nil {
		return err
	}

	paths, err := asObject(doc["paths"], "$.paths")
	if err != nil {
		return err
	}
	expectedByPath := make(map[string]map[string]operationExpectation)
	for _, expectation := range expectedOperations {
		if expectedByPath[expectation.path] == nil {
			expectedByPath[expectation.path] = make(map[string]operationExpectation)
		}
		if _, exists := expectedByPath[expectation.path][expectation.method]; exists {
			return fmt.Errorf("duplicate checker expectation for %s %s", expectation.method, expectation.path)
		}
		expectedByPath[expectation.path][expectation.method] = expectation
	}
	if len(paths) != len(expectedByPath) {
		return fmt.Errorf("$.paths must contain the S1, S2, and S3 operation set (%d paths), got %d", len(expectedByPath), len(paths))
	}
	for path := range paths {
		if _, ok := expectedByPath[path]; !ok {
			return fmt.Errorf("$.paths contains unexpected path %q", path)
		}
	}

	pathNames := make([]string, 0, len(expectedByPath))
	for path := range expectedByPath {
		pathNames = append(pathNames, path)
	}
	sort.Strings(pathNames)
	for _, path := range pathNames {
		pathItem, err := asObject(paths[path], "$.paths["+path+"]")
		if err != nil {
			return err
		}
		methods := make([]string, 0, len(expectedByPath[path]))
		for method := range expectedByPath[path] {
			methods = append(methods, method)
		}
		sort.Strings(methods)
		for _, method := range methods {
			expectation := expectedByPath[path][method]
			operationValue, ok := pathItem[method]
			if !ok {
				return fmt.Errorf("$.paths[%s] must define %s", path, strings.ToUpper(method))
			}
			operation, err := asObject(operationValue, "$.paths["+path+"]."+method)
			if err != nil {
				return err
			}
			operationID, err := asString(operation["operationId"], "$.paths["+path+"]."+method+".operationId")
			if err != nil || operationID == "" {
				return fmt.Errorf("$.paths[%s].%s.operationId is required", path, method)
			}
			if err := checkOperationSecurity(operation, path, method, expectation.security); err != nil {
				return err
			}
			if err := checkOperationResponses(operation, path, method); err != nil {
				return err
			}
		}
	}

	metaPath, err := asObject(paths["/api/v1/meta"], "$.paths[/api/v1/meta]")
	if err != nil {
		return err
	}
	metaOperation, err := asObject(metaPath["get"], "$.paths[/api/v1/meta].get")
	if err != nil {
		return err
	}
	metaResponses, err := asObject(metaOperation["responses"], "$.paths[/api/v1/meta].get.responses")
	if err != nil {
		return err
	}
	okResponse, err := requiredObject(metaResponses, "200", "$.paths[/api/v1/meta].get.responses")
	if err != nil {
		return err
	}
	content, err := requiredObject(okResponse, "content", "$.paths[/api/v1/meta].get.responses[200]")
	if err != nil {
		return err
	}
	jsonContent, err := requiredObject(content, "application/json", "$.paths[/api/v1/meta].get.responses[200].content")
	if err != nil {
		return err
	}
	schema, err := requiredObject(jsonContent, "schema", "$.paths[/api/v1/meta].get.responses[200].content[application/json]")
	if err != nil {
		return err
	}
	metaRef, err := asString(schema["$ref"], "$.paths[/api/v1/meta].get.responses[200].content[application/json].schema.$ref")
	if err != nil {
		return err
	}
	if metaRef != "./schemas/meta.schema.json" {
		return fmt.Errorf("meta response must reference ./schemas/meta.schema.json, got %q", metaRef)
	}

	components, err := requiredObject(doc, "components", "$")
	if err != nil {
		return err
	}
	componentSchemas, err := requiredObject(components, "schemas", "$.components")
	if err != nil {
		return err
	}
	expectedComponentSchemas := map[string]string{
		"MetaResponse":                       "./schemas/meta.schema.json",
		"ErrorEnvelope":                      "./schemas/error.schema.json",
		"DevicePlatform":                     "./schemas/device-platform.schema.json",
		"DeviceAuthorizationState":           "./schemas/device-authorization-state.schema.json",
		"DeviceAuthorizationStartRequest":    "./schemas/device-authorization-start-request.schema.json",
		"DeviceAuthorizationStart":           "./schemas/device-authorization-start.schema.json",
		"DeviceAuthorizationStatus":          "./schemas/device-authorization-status.schema.json",
		"DeviceAuthorizationLookup":          "./schemas/device-authorization-lookup.schema.json",
		"DeviceAuthorizationDecisionRequest": "./schemas/device-authorization-decision-request.schema.json",
		"DeviceAuthorizationDecision":        "./schemas/device-authorization-decision.schema.json",
		"DeviceTokenRequest":                 "./schemas/device-token-request.schema.json",
		"DeviceTokenPending":                 "./schemas/device-token-pending.schema.json",
		"DeviceTokenPair":                    "./schemas/auth-token-pair.schema.json",
		"RefreshTokenRequest":                "./schemas/refresh-token-request.schema.json",
		"NativeDevice":                       "./schemas/device.schema.json",
		"DeviceList":                         "./schemas/device-list.schema.json",
		"DeviceRevocation":                   "./schemas/device-revocation.schema.json",
		"AuthenticatedUser":                  "./schemas/auth-user.schema.json",
		"PlaybackTicketState":                "./schemas/playback-ticket-state.schema.json",
		"PlaybackTransportKind":              "./schemas/playback-transport-kind.schema.json",
		"PlaybackClientCapabilities":         "./schemas/playback-client-capabilities.schema.json",
		"PlaybackTicketCreateRequest":        "./schemas/playback-ticket-create-request.schema.json",
		"PlaybackTicket":                     "./schemas/playback-ticket.schema.json",
		"PlaybackTicketIssuance":             "./schemas/playback-ticket-issuance.schema.json",
		"PlaybackFailureCode":                "./schemas/playback-failure-code.schema.json",
		"PlaybackTransport":                  "./schemas/transport.schema.json",
		"PlaybackDirectDASHStream":           "./schemas/direct-dash-stream.schema.json",
		"PlaybackDirectDASHTransport":        "./schemas/transport-direct-dash.schema.json",
		"PlaybackMediaSummary":               "./schemas/media.schema.json",
		"PlaybackStream":                     "./schemas/stream.schema.json",
		"PlaybackStreamHeaders":              "./schemas/stream-headers.schema.json",
		"MediaSession":                       "./schemas/media-session.schema.json",
		"PlaybackSubtitle":                   "./schemas/subtitle.schema.json",
	}
	if len(componentSchemas) != len(expectedComponentSchemas) {
		return fmt.Errorf("$.components.schemas must contain exactly %d documented schemas, got %d",
			len(expectedComponentSchemas), len(componentSchemas))
	}
	for name := range componentSchemas {
		if _, ok := expectedComponentSchemas[name]; !ok {
			return fmt.Errorf("$.components.schemas contains unexpected schema %q", name)
		}
	}
	for name, wantRef := range expectedComponentSchemas {
		component, err := requiredObject(componentSchemas, name, "$.components.schemas")
		if err != nil {
			return err
		}
		ref, err := asString(component["$ref"], "$.components.schemas."+name+".$ref")
		if err != nil {
			return err
		}
		if ref != wantRef {
			return fmt.Errorf("components.schemas.%s must reference %s, got %q", name, wantRef, ref)
		}
	}

	securitySchemes, err := requiredObject(components, "securitySchemes", "$.components")
	if err != nil {
		return err
	}
	bearerAuth, err := requiredObject(securitySchemes, "bearerAuth", "$.components.securitySchemes")
	if err != nil {
		return err
	}
	if scheme, _ := bearerAuth["scheme"].(string); scheme != "bearer" {
		return fmt.Errorf("components.securitySchemes.bearerAuth.scheme must be bearer")
	}
	if schemeType, _ := bearerAuth["type"].(string); schemeType != "http" {
		return fmt.Errorf("components.securitySchemes.bearerAuth.type must be http")
	}
	webSession, err := requiredObject(securitySchemes, "webSessionCookie", "$.components.securitySchemes")
	if err != nil {
		return err
	}
	if schemeType, _ := webSession["type"].(string); schemeType != "apiKey" {
		return fmt.Errorf("components.securitySchemes.webSessionCookie.type must be apiKey")
	}
	if in, _ := webSession["in"].(string); in != "cookie" {
		return fmt.Errorf("components.securitySchemes.webSessionCookie.in must be cookie")
	}
	if name, _ := webSession["name"].(string); name != "doriyamedia_token" {
		return fmt.Errorf("components.securitySchemes.webSessionCookie.name must be doriyamedia_token")
	}
	webBearer, err := requiredObject(securitySchemes, "webBearerAuth", "$.components.securitySchemes")
	if err != nil {
		return err
	}
	if schemeType, _ := webBearer["type"].(string); schemeType != "http" {
		return fmt.Errorf("components.securitySchemes.webBearerAuth.type must be http")
	}
	if scheme, _ := webBearer["scheme"].(string); scheme != "bearer" {
		return fmt.Errorf("components.securitySchemes.webBearerAuth.scheme must be bearer")
	}

	componentResponses, err := requiredObject(components, "responses", "$.components")
	if err != nil {
		return err
	}
	stableError, err := requiredObject(componentResponses, "StableError", "$.components.responses")
	if err != nil {
		return err
	}
	errorContent, err := requiredObject(stableError, "content", "$.components.responses.StableError")
	if err != nil {
		return err
	}
	errorJSON, err := requiredObject(errorContent, "application/json", "$.components.responses.StableError.content")
	if err != nil {
		return err
	}
	errorSchema, err := requiredObject(errorJSON, "schema", "$.components.responses.StableError.content[application/json]")
	if err != nil {
		return err
	}
	errorRef, err := asString(errorSchema["$ref"], "$.components.responses.StableError.content[application/json].schema.$ref")
	if err != nil {
		return err
	}
	if errorRef != "#/components/schemas/ErrorEnvelope" {
		return fmt.Errorf("StableError must reference #/components/schemas/ErrorEnvelope")
	}
	if err := checkPlaybackOperations(doc, paths, components); err != nil {
		return err
	}

	return nil
}

func checkPlaybackOperations(doc map[string]any, paths, components map[string]any) error {
	topCapability, err := requiredObject(doc, "x-media-session-capability", "$")
	if err != nil {
		return err
	}
	if location, _ := topCapability["location"].(string); location != "path" {
		return fmt.Errorf("$.x-media-session-capability.location must be path")
	}
	if parameter, _ := topCapability["parameter"].(string); parameter != "id" {
		return fmt.Errorf("$.x-media-session-capability.parameter must be id")
	}
	if redact, _ := topCapability["logRedaction"].(string); redact != "required" {
		return fmt.Errorf("$.x-media-session-capability.logRedaction must be required")
	}

	create, err := playbackOperation(paths, "/api/v1/playback/tickets", "post")
	if err != nil {
		return err
	}
	if err := checkJSONRequestRef(create, "/api/v1/playback/tickets", "post", "./schemas/playback-ticket-create-request.schema.json"); err != nil {
		return err
	}
	if err := checkJSONResponseRef(create, "/api/v1/playback/tickets", "post", "201", "./schemas/playback-ticket-issuance.schema.json"); err != nil {
		return err
	}
	if err := checkJSONResponseRef(create, "/api/v1/playback/tickets", "post", "202", "./schemas/playback-ticket.schema.json"); err != nil {
		return err
	}
	if err := checkNoStoreHeader(create, "201", "POST /api/v1/playback/tickets"); err != nil {
		return err
	}
	if err := checkOptionalTicketLocationHeader(
		create, "500", "POST /api/v1/playback/tickets"); err != nil {
		return err
	}
	if err := checkJSONResponseRef(create, "/api/v1/playback/tickets", "post", "500", "#/components/schemas/ErrorEnvelope"); err != nil {
		return err
	}
	if err := checkPlaybackDeadlineResponses(
		create, "POST /api/v1/playback/tickets"); err != nil {
		return err
	}

	status, err := playbackOperation(paths, "/api/v1/playback/tickets/{id}", "get")
	if err != nil {
		return err
	}
	if err := checkJSONResponseRef(status, "/api/v1/playback/tickets/{id}", "get", "200", "./schemas/playback-ticket.schema.json"); err != nil {
		return err
	}
	if err := checkPlaybackDeadlineResponses(
		status, "GET /api/v1/playback/tickets/{id}"); err != nil {
		return err
	}

	refresh, err := playbackOperation(paths, "/api/v1/playback/tickets/{id}/refresh", "post")
	if err != nil {
		return err
	}
	if _, exists := refresh["requestBody"]; exists {
		return fmt.Errorf("POST /api/v1/playback/tickets/{id}/refresh must not accept a request body")
	}
	if err := checkJSONResponseRef(refresh, "/api/v1/playback/tickets/{id}/refresh", "post", "200", "./schemas/playback-ticket-issuance.schema.json"); err != nil {
		return err
	}
	if err := checkNoStoreHeader(refresh, "200", "POST /api/v1/playback/tickets/{id}/refresh"); err != nil {
		return err
	}
	if err := checkPlaybackDeadlineResponses(
		refresh, "POST /api/v1/playback/tickets/{id}/refresh"); err != nil {
		return err
	}
	if err := checkPlaybackRefreshSemantics(doc); err != nil {
		return err
	}

	mediaResponses, err := asObject(components["responses"], "$.components.responses")
	if err != nil {
		return err
	}
	stream, err := requiredObject(mediaResponses, "ServerProxyStream", "$.components.responses")
	if err != nil {
		return err
	}
	streamContent, err := requiredObject(stream, "content", "$.components.responses.ServerProxyStream")
	if err != nil {
		return err
	}
	binaryContent, err := requiredObject(streamContent, "application/octet-stream", "$.components.responses.ServerProxyStream.content")
	if err != nil {
		return err
	}
	binarySchema, err := requiredObject(binaryContent, "schema", "$.components.responses.ServerProxyStream.content[application/octet-stream]")
	if err != nil {
		return err
	}
	if format, _ := binarySchema["format"].(string); format != "binary" {
		return fmt.Errorf("ServerProxyStream must declare a binary response schema")
	}

	for _, method := range []string{"get", "head"} {
		operation, err := playbackOperation(paths, "/api/v1/playback/media-sessions/{id}/stream", method)
		if err != nil {
			return err
		}
		if _, exists := operation["requestBody"]; exists {
			return fmt.Errorf("%s /api/v1/playback/media-sessions/{id}/stream must not accept a request body", strings.ToUpper(method))
		}
		parameters, err := asArray(operation["parameters"], "$.paths[/api/v1/playback/media-sessions/{id}/stream]."+method+".parameters")
		if err != nil {
			return err
		}
		if len(parameters) != 3 {
			return fmt.Errorf("%s media stream must declare MediaSessionId, Range, and IfRange parameters", strings.ToUpper(method))
		}
		if method == "head" {
			responses, err := asObject(operation["responses"], "$.paths[/api/v1/playback/media-sessions/{id}/stream].head.responses")
			if err != nil {
				return err
			}
			for code, raw := range responses {
				response, err := asObject(raw, "$.paths[/api/v1/playback/media-sessions/{id}/stream].head.responses["+code+"]")
				if err != nil {
					return err
				}
				if _, exists := response["content"]; exists {
					return fmt.Errorf("HEAD media response %s must not declare a response body", code)
				}
			}
			if _, ok := responses["200"]; !ok {
				return fmt.Errorf("HEAD media stream must declare 200")
			}
			if _, ok := responses["206"]; !ok {
				return fmt.Errorf("HEAD media stream must declare 206")
			}
		}
	}

	parameters, err := asObject(components["parameters"], "$.components")
	if err != nil {
		return err
	}
	mediaID, err := requiredObject(parameters, "MediaSessionId", "$.components.parameters")
	if err != nil {
		return err
	}
	mediaIDSchema, err := requiredObject(mediaID, "schema", "$.components.parameters.MediaSessionId")
	if err != nil {
		return err
	}
	if pattern, _ := mediaIDSchema["pattern"].(string); pattern != "^ms_[A-Za-z0-9_-]{22,96}$" {
		return fmt.Errorf("MediaSessionId must use the ms_ capability pattern")
	}
	if err := checkPlaybackHLSOperations(paths, mediaResponses, parameters); err != nil {
		return err
	}
	return nil
}

func checkPlaybackHLSOperations(paths, responses, parameters map[string]any) error {
	for _, tc := range []struct {
		path       string
		parameter  string
		response   string
		mediaType  string
		parameterN int
	}{
		{
			path:       "/api/v1/playback/media-sessions/{id}/index.m3u8",
			response:   "ServerHLSPlaylist",
			mediaType:  "application/vnd.apple.mpegurl",
			parameterN: 1,
		},
		{
			path:       "/api/v1/playback/media-sessions/{id}/{segment}",
			parameter:  "HLSSegment",
			response:   "ServerHLSSegment",
			mediaType:  "video/mp2t",
			parameterN: 2,
		},
	} {
		operation, err := playbackOperation(paths, tc.path, "get")
		if err != nil {
			return err
		}
		if _, exists := operation["requestBody"]; exists {
			return fmt.Errorf("GET %s must not accept a request body", tc.path)
		}
		if queryPolicy, _ := operation["x-query-parameters"].(string); queryPolicy != "forbidden" {
			return fmt.Errorf("GET %s must forbid query parameters", tc.path)
		}
		methodPolicy, err := requiredObject(
			operation,
			"x-http-method-policy",
			"$.paths["+tc.path+"].get")
		if err != nil {
			return err
		}
		allowedMethods, err := asArray(
			methodPolicy["allowed"],
			"$.paths["+tc.path+"].get.x-http-method-policy.allowed")
		if err != nil {
			return err
		}
		if len(allowedMethods) != 1 {
			return fmt.Errorf("GET %s must allow exactly GET", tc.path)
		}
		allowed, err := asString(
			allowedMethods[0],
			"$.paths["+tc.path+"].get.x-http-method-policy.allowed[0]")
		if err != nil {
			return err
		}
		if allowed != http.MethodGet {
			return fmt.Errorf("GET %s must allow exactly GET", tc.path)
		}
		headStatus, err := asInteger(
			methodPolicy["HEAD"],
			"$.paths["+tc.path+"].get.x-http-method-policy.HEAD")
		if err != nil {
			return err
		}
		if headStatus != http.StatusMethodNotAllowed {
			return fmt.Errorf("HEAD %s must be 405", tc.path)
		}
		headAllow, err := asString(
			methodPolicy["HEAD_allow"],
			"$.paths["+tc.path+"].get.x-http-method-policy.HEAD_allow")
		if err != nil {
			return err
		}
		if headAllow != http.MethodGet {
			return fmt.Errorf("HEAD %s must declare Allow: GET", tc.path)
		}
		params, err := asArray(operation["parameters"], "$.paths["+tc.path+"].get.parameters")
		if err != nil {
			return err
		}
		if len(params) != tc.parameterN {
			return fmt.Errorf("GET %s must declare %d parameters", tc.path, tc.parameterN)
		}

		operationResponses, err := asObject(operation["responses"], "$.paths["+tc.path+"].get.responses")
		if err != nil {
			return err
		}
		rawSuccess, err := requiredObject(operationResponses, "200", "$.paths["+tc.path+"].get.responses")
		if err != nil {
			return err
		}
		if ref, _ := rawSuccess["$ref"].(string); ref != "#/components/responses/"+tc.response {
			return fmt.Errorf("GET %s 200 must reference %s", tc.path, tc.response)
		}
		response, err := requiredObject(responses, tc.response, "$.components.responses")
		if err != nil {
			return err
		}
		content, err := requiredObject(response, "content", "$.components.responses."+tc.response)
		if err != nil {
			return err
		}
		if _, ok := content[tc.mediaType]; !ok {
			return fmt.Errorf("%s must declare %s content", tc.response, tc.mediaType)
		}
		headers, err := requiredObject(response, "headers", "$.components.responses."+tc.response)
		if err != nil {
			return err
		}
		cache, err := requiredObject(headers, "Cache-Control", "$.components.responses."+tc.response+".headers")
		if err != nil {
			return err
		}
		cacheSchema, err := requiredObject(cache, "schema", "$.components.responses."+tc.response+".headers.Cache-Control")
		if err != nil {
			return err
		}
		if value, _ := cacheSchema["const"].(string); value != "private, no-store" {
			return fmt.Errorf("%s Cache-Control must be private, no-store", tc.response)
		}
	}

	segment, err := requiredObject(parameters, "HLSSegment", "$.components.parameters")
	if err != nil {
		return err
	}
	segmentSchema, err := requiredObject(segment, "schema", "$.components.parameters.HLSSegment")
	if err != nil {
		return err
	}
	if pattern, _ := segmentSchema["pattern"].(string); pattern != "^[0-9]+\\.ts$" {
		return fmt.Errorf("HLSSegment must use the numbered .ts pattern")
	}
	return nil
}

func contractOperation(paths map[string]any, path, method string) (map[string]any, error) {
	pathItem, err := asObject(paths[path], "$.paths["+path+"]")
	if err != nil {
		return nil, err
	}
	operation, err := asObject(pathItem[method], "$.paths["+path+"]."+method)
	if err != nil {
		return nil, err
	}
	return operation, nil
}

func checkPlaybackRefreshSemantics(doc map[string]any) error {
	semantics, err := requiredObject(doc, "x-playback-ticket-semantics", "$")
	if err != nil {
		return err
	}
	refresh, err := requiredObject(
		semantics, "refresh", "$.x-playback-ticket-semantics")
	if err != nil {
		return err
	}
	allowedRaw, err := asArray(
		refresh["allowed_states"],
		"$.x-playback-ticket-semantics.refresh.allowed_states")
	if err != nil {
		return err
	}
	allowed := make([]string, 0, len(allowedRaw))
	for i, raw := range allowedRaw {
		value, err := asString(raw,
			fmt.Sprintf("$.x-playback-ticket-semantics.refresh.allowed_states[%d]", i))
		if err != nil {
			return err
		}
		allowed = append(allowed, value)
	}
	if !sameStringSet(allowed, []string{"ready", "consuming"}) || len(allowed) != 2 {
		return fmt.Errorf("$.x-playback-ticket-semantics.refresh.allowed_states must be exactly ready and consuming")
	}
	if gate, _ := refresh["state_gate_before_source_access"].(string); gate != "required" {
		return fmt.Errorf("$.x-playback-ticket-semantics.refresh.state_gate_before_source_access must be required")
	}
	if probe, _ := refresh["source_probe"].(string); probe != "forbidden" {
		return fmt.Errorf("$.x-playback-ticket-semantics.refresh.source_probe must be forbidden")
	}
	oneDescription, err := asBool(
		refresh["one_transport_description_per_successful_response"],
		"$.x-playback-ticket-semantics.refresh.one_transport_description_per_successful_response")
	if err != nil || !oneDescription {
		return fmt.Errorf("$.x-playback-ticket-semantics.refresh.one_transport_description_per_successful_response must be true")
	}
	if claim, _ := refresh["concurrent_claim"].(string); claim != "exactly_one_winner" {
		return fmt.Errorf("$.x-playback-ticket-semantics.refresh.concurrent_claim must be exactly_one_winner")
	}
	singleFlight, err := asBool(
		refresh["client_single_flight_required"],
		"$.x-playback-ticket-semantics.refresh.client_single_flight_required")
	if err != nil || !singleFlight {
		return fmt.Errorf("$.x-playback-ticket-semantics.refresh.client_single_flight_required must be true")
	}

	checkOutcome := func(
		key string,
		wantStatus int64,
		wantCode, wantAction string,
	) error {
		path := "$.x-playback-ticket-semantics.refresh." + key
		outcome, err := requiredObject(
			refresh, key, "$.x-playback-ticket-semantics.refresh")
		if err != nil {
			return err
		}
		status, err := asInteger(outcome["status"], path+".status")
		if err != nil || status != wantStatus {
			return fmt.Errorf("%s.status must be %d", path, wantStatus)
		}
		if code, _ := outcome["code"].(string); code != wantCode {
			return fmt.Errorf("%s.code must be %s", path, wantCode)
		}
		if action, _ := outcome["action"].(string); action != wantAction {
			return fmt.Errorf("%s.action must be %s", path, wantAction)
		}
		return nil
	}
	if err := checkOutcome(
		"probing", http.StatusConflict, "invalid_request", "status"); err != nil {
		return err
	}
	if err := checkOutcome(
		"revoked", http.StatusForbidden, "forbidden", "reauthorize"); err != nil {
		return err
	}

	terminal, err := requiredObject(
		refresh, "terminal", "$.x-playback-ticket-semantics.refresh")
	if err != nil {
		return err
	}
	terminalRaw, err := asArray(
		terminal["states"],
		"$.x-playback-ticket-semantics.refresh.terminal.states")
	if err != nil {
		return err
	}
	terminalStates := make([]string, 0, len(terminalRaw))
	for i, raw := range terminalRaw {
		value, err := asString(raw,
			fmt.Sprintf("$.x-playback-ticket-semantics.refresh.terminal.states[%d]", i))
		if err != nil {
			return err
		}
		terminalStates = append(terminalStates, value)
	}
	wantTerminal := []string{"unsupported", "failed", "expired", "exhausted"}
	if !sameStringSet(terminalStates, wantTerminal) ||
		len(terminalStates) != len(wantTerminal) {
		return fmt.Errorf(
			"$.x-playback-ticket-semantics.refresh.terminal.states must be exactly %v",
			wantTerminal)
	}
	if status, err := asInteger(
		terminal["status"],
		"$.x-playback-ticket-semantics.refresh.terminal.status"); err != nil ||
		status != http.StatusGone {
		return fmt.Errorf("$.x-playback-ticket-semantics.refresh.terminal.status must be 410")
	}
	if code, _ := terminal["code"].(string); code != "ticket_expired" {
		return fmt.Errorf("$.x-playback-ticket-semantics.refresh.terminal.code must be ticket_expired")
	}
	if action, _ := terminal["action"].(string); action != "request_new_ticket" {
		return fmt.Errorf("$.x-playback-ticket-semantics.refresh.terminal.action must be request_new_ticket")
	}
	return nil
}

func playbackOperation(paths map[string]any, path, method string) (map[string]any, error) {
	return contractOperation(paths, path, method)
}

func checkJSONRequestRef(operation map[string]any, path, method, wantRef string) error {
	requestBody, err := requiredObject(operation, "requestBody", strings.ToUpper(method)+" "+path)
	if err != nil {
		return err
	}
	content, err := requiredObject(requestBody, "content", strings.ToUpper(method)+" "+path+".requestBody")
	if err != nil {
		return err
	}
	jsonContent, err := requiredObject(content, "application/json", strings.ToUpper(method)+" "+path+".requestBody.content")
	if err != nil {
		return err
	}
	schema, err := requiredObject(jsonContent, "schema", strings.ToUpper(method)+" "+path+".requestBody.content[application/json]")
	if err != nil {
		return err
	}
	ref, err := asString(schema["$ref"], strings.ToUpper(method)+" "+path+" request schema")
	if err != nil {
		return err
	}
	if ref != wantRef {
		return fmt.Errorf("%s %s request must reference %s, got %q", strings.ToUpper(method), path, wantRef, ref)
	}
	return nil
}

func checkJSONResponseRef(operation map[string]any, path, method, status, wantRef string) error {
	responses, err := asObject(operation["responses"], strings.ToUpper(method)+" "+path+".responses")
	if err != nil {
		return err
	}
	response, err := requiredObject(responses, status, strings.ToUpper(method)+" "+path+".responses")
	if err != nil {
		return err
	}
	content, err := requiredObject(response, "content", strings.ToUpper(method)+" "+path+".responses["+status+"]")
	if err != nil {
		return err
	}
	jsonContent, err := requiredObject(content, "application/json", strings.ToUpper(method)+" "+path+".responses["+status+"].content")
	if err != nil {
		return err
	}
	schema, err := requiredObject(jsonContent, "schema", strings.ToUpper(method)+" "+path+".responses["+status+"].content[application/json]")
	if err != nil {
		return err
	}
	ref, err := asString(schema["$ref"], strings.ToUpper(method)+" "+path+" response schema")
	if err != nil {
		return err
	}
	if ref != wantRef {
		return fmt.Errorf("%s %s %s response must reference %s, got %q", strings.ToUpper(method), path, status, wantRef, ref)
	}
	return nil
}

func checkNoStoreHeader(operation map[string]any, status, label string) error {
	responses, err := asObject(operation["responses"], label+".responses")
	if err != nil {
		return err
	}
	response, err := requiredObject(responses, status, label+".responses")
	if err != nil {
		return err
	}
	headers, err := requiredObject(response, "headers", label+".responses["+status+"]")
	if err != nil {
		return err
	}
	cacheControl, err := requiredObject(headers, "Cache-Control", label+".responses["+status+"].headers")
	if err != nil {
		return err
	}
	schema, err := requiredObject(cacheControl, "schema", label+".responses["+status+"].headers.Cache-Control")
	if err != nil {
		return err
	}
	if value, _ := schema["const"].(string); value != "no-store" {
		return fmt.Errorf("%s %s Cache-Control must be no-store", label, status)
	}
	return nil
}

func checkOptionalTicketLocationHeader(
	operation map[string]any,
	status, label string,
) error {
	responses, err := asObject(operation["responses"], label+".responses")
	if err != nil {
		return err
	}
	response, err := requiredObject(responses, status, label+".responses")
	if err != nil {
		return err
	}
	headers, err := requiredObject(
		response, "headers", label+".responses["+status+"]")
	if err != nil {
		return err
	}
	location, err := requiredObject(
		headers, "Location", label+".responses["+status+"].headers")
	if err != nil {
		return err
	}
	schema, err := requiredObject(
		location, "schema",
		label+".responses["+status+"].headers.Location")
	if err != nil {
		return err
	}
	if schemaType, _ := schema["type"].(string); schemaType != "string" {
		return fmt.Errorf("%s %s Location.schema.type must be string", label, status)
	}
	if pattern, _ := schema["pattern"].(string); pattern != "^/api/v1/playback/tickets/pt_[A-Za-z0-9_-]{8,96}$" {
		return fmt.Errorf("%s %s Location.schema.pattern is invalid", label, status)
	}
	return nil
}

func checkPlaybackDeadlineResponses(operation map[string]any, label string) error {
	responses, err := asObject(operation["responses"], label+".responses")
	if err != nil {
		return err
	}
	for _, status := range []string{"408", "504"} {
		raw, ok := responses[status]
		if !ok {
			return fmt.Errorf("%s must declare %s", label, status)
		}
		response, err := asObject(raw, label+".responses["+status+"]")
		if err != nil {
			return err
		}
		if ref, _ := response["$ref"].(string); ref != "#/components/responses/StableError" {
			return fmt.Errorf("%s %s must use #/components/responses/StableError", label, status)
		}
	}
	return nil
}

func checkOperationSecurity(operation map[string]any, path, method, want string) error {
	label := strings.ToUpper(method) + " " + path
	raw, ok := operation["security"]
	if !ok {
		return fmt.Errorf("%s.security is required", label)
	}
	requirements, ok := raw.([]any)
	if !ok {
		return fmt.Errorf("%s.security must be an array", label)
	}
	if want == "public" {
		if len(requirements) != 0 {
			return fmt.Errorf("%s must be public with security: []", label)
		}
		return nil
	}
	if want == "mediaCapability" {
		if len(requirements) != 0 {
			return fmt.Errorf("%s must accept no bearer or cookie credential", label)
		}
		return checkPathCapability(operation, label, "x-media-session-capability", "id", "media-session")
	}
	if want == "subtitleCapability" {
		if len(requirements) != 0 {
			return fmt.Errorf("%s must accept no bearer or cookie credential", label)
		}
		return checkPathCapability(operation, label, "x-playback-subtitle-capability", "id", "subtitle")
	}
	if want == "mediaAndSubtitleCapability" {
		if len(requirements) != 0 {
			return fmt.Errorf("%s must accept no bearer or cookie credential", label)
		}
		if err := checkPathCapability(
			operation, label, "x-media-session-capability", "id", "media-session"); err != nil {
			return err
		}
		return checkPathCapability(
			operation, label, "x-playback-subtitle-capability", "subtitle", "subtitle")
	}
	if want == "webSession" {
		if len(requirements) != 2 {
			return fmt.Errorf("%s must allow webSessionCookie and webBearerAuth", label)
		}
		seen := map[string]bool{}
		for _, rawRequirement := range requirements {
			requirement, ok := rawRequirement.(map[string]any)
			if !ok || len(requirement) != 1 {
				return fmt.Errorf("%s Web session alternatives must each name one scheme", label)
			}
			for _, scheme := range []string{"webSessionCookie", "webBearerAuth"} {
				if scopes, ok := requirement[scheme].([]any); ok {
					if len(scopes) != 0 {
						return fmt.Errorf("%s %s must have no scopes", label, scheme)
					}
					seen[scheme] = true
					break
				}
			}
		}
		if !seen["webSessionCookie"] || !seen["webBearerAuth"] {
			return fmt.Errorf("%s must allow webSessionCookie and webBearerAuth", label)
		}
		return nil
	}
	if len(requirements) != 1 {
		return fmt.Errorf("%s must use exactly %s", label, want)
	}
	requirement, ok := requirements[0].(map[string]any)
	if !ok || len(requirement) != 1 {
		return fmt.Errorf("%s must require only %s", label, want)
	}
	scopes, ok := requirement[want].([]any)
	if !ok || len(scopes) != 0 {
		return fmt.Errorf("%s must require %s with no scopes", label, want)
	}
	return nil
}

func checkPathCapability(
	operation map[string]any,
	label, extension, parameter, kind string,
) error {
	capability, err := requiredObject(operation, extension, label)
	if err != nil {
		return err
	}
	if location, _ := capability["location"].(string); location != "path" {
		return fmt.Errorf("%s %s capability location must be path", label, kind)
	}
	if got, _ := capability["parameter"].(string); got != parameter {
		return fmt.Errorf("%s %s capability parameter must be %s", label, kind, parameter)
	}
	if redact, ok := capability["log_redaction"].(bool); !ok || !redact {
		return fmt.Errorf("%s must require %s path log redaction", label, kind)
	}
	return nil
}

func checkOperationResponses(operation map[string]any, path, method string) error {
	responses, err := asObject(operation["responses"], "$.paths["+path+"].responses")
	if err != nil {
		return err
	}
	successCount := 0
	for code, responseValue := range responses {
		response, err := asObject(responseValue, "$.paths["+path+"].responses["+code+"]")
		if err != nil {
			return err
		}
		if isSuccessStatus(code) {
			successCount++
			continue
		}
		if method == "head" {
			continue
		}
		ref, hasRef := response["$ref"].(string)
		if hasRef && ref == "#/components/responses/StableError" {
			continue
		}
		if usesStableErrorSchema(response) {
			continue
		}
		if hasRef {
			return fmt.Errorf("%s %s response %s must use #/components/responses/StableError", strings.ToUpper(method), path, code)
		}
		return fmt.Errorf("%s %s response %s must use the StableError envelope", strings.ToUpper(method), path, code)
	}
	if successCount == 0 {
		return fmt.Errorf("$.paths[%s] must define at least one success response", path)
	}
	if _, ok := responses["default"]; !ok {
		return fmt.Errorf("$.paths[%s].responses must define default", path)
	}
	return nil
}

func usesStableErrorSchema(response map[string]any) bool {
	content, ok := response["content"].(map[string]any)
	if !ok {
		return false
	}
	jsonContent, ok := content["application/json"].(map[string]any)
	if !ok {
		return false
	}
	schema, ok := jsonContent["schema"].(map[string]any)
	if !ok {
		return false
	}
	ref, _ := schema["$ref"].(string)
	return ref == "#/components/schemas/ErrorEnvelope"
}

func isSuccessStatus(code string) bool {
	return len(code) == 3 && code[0] == '2'
}

func (c *checker) checkRefs(openapiPath string, openapiDoc any) error {
	docs := map[string]any{
		filepath.Clean(openapiPath): openapiDoc,
	}
	schemaPaths, err := filepath.Glob(filepath.Join(c.protocolDir, "schemas", "*.schema.json"))
	if err != nil {
		return err
	}
	sort.Strings(schemaPaths)
	for _, path := range schemaPaths {
		doc, err := readJSON(path)
		if err != nil {
			return fmt.Errorf("%s: %w", c.rel(path), err)
		}
		docs[filepath.Clean(path)] = doc
	}

	var occurrences []refOccurrence
	for _, owner := range append([]string{openapiPath}, schemaPaths...) {
		owner = filepath.Clean(owner)
		collectRefs(docs[owner], "$", owner, &occurrences)
	}

	var problems []string
	for _, occurrence := range occurrences {
		if strings.TrimSpace(occurrence.ref) == "" {
			problems = append(problems, fmt.Sprintf("%s:%s has an empty $ref", c.rel(occurrence.owner), occurrence.path))
			continue
		}
		if _, err := c.resolveRef(occurrence, docs); err != nil {
			problems = append(problems, fmt.Sprintf("%s:%s: %v", c.rel(occurrence.owner), occurrence.path, err))
		}
	}
	c.refCount = len(occurrences)
	if len(problems) > 0 {
		return fmt.Errorf("invalid $ref targets:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

func collectRefs(value any, path, owner string, out *[]refOccurrence) {
	switch v := value.(type) {
	case map[string]any:
		if ref, ok := v["$ref"].(string); ok {
			*out = append(*out, refOccurrence{owner: owner, path: path, ref: ref})
		}
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			collectRefs(v[key], path+"."+key, owner, out)
		}
	case []any:
		for i, item := range v {
			collectRefs(item, fmt.Sprintf("%s[%d]", path, i), owner, out)
		}
	}
}

func (c *checker) resolveRef(occurrence refOccurrence, docs map[string]any) (any, error) {
	target := filepath.Clean(occurrence.owner)
	fragment := ""
	refPath, refFragment, hasFragment := strings.Cut(occurrence.ref, "#")
	if hasFragment {
		fragment = refFragment
	}
	if refPath != "" {
		decoded, err := url.PathUnescape(refPath)
		if err != nil {
			return nil, fmt.Errorf("invalid ref path %q: %w", occurrence.ref, err)
		}
		parsed, err := url.Parse(decoded)
		if err != nil {
			return nil, fmt.Errorf("invalid ref %q: %w", occurrence.ref, err)
		}
		if parsed.Scheme != "" {
			return nil, fmt.Errorf("external URL refs are not allowed in protocol assets: %q", occurrence.ref)
		}
		target = filepath.Clean(filepath.Join(filepath.Dir(occurrence.owner), filepath.FromSlash(parsed.Path)))
	}

	rel, err := filepath.Rel(c.protocolDir, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("ref %q escapes protocol/", occurrence.ref)
	}
	if _, err := os.Stat(target); err != nil {
		return nil, fmt.Errorf("ref %q target does not exist: %s", occurrence.ref, c.rel(target))
	}

	doc, ok := docs[target]
	if !ok {
		doc, err = readJSON(target)
		if err != nil {
			return nil, fmt.Errorf("reading ref target %s: %w", c.rel(target), err)
		}
		docs[target] = doc
	}
	if fragment == "" {
		return doc, nil
	}

	decodedFragment, err := url.PathUnescape(fragment)
	if err != nil {
		return nil, fmt.Errorf("invalid ref fragment %q: %w", occurrence.ref, err)
	}
	return resolveJSONPointer(doc, decodedFragment)
}

func resolveJSONPointer(doc any, pointer string) (any, error) {
	if pointer == "" {
		return doc, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("unsupported non-JSON-pointer fragment %q", pointer)
	}

	current := doc
	for _, raw := range strings.Split(pointer[1:], "/") {
		token := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		switch value := current.(type) {
		case map[string]any:
			next, ok := value[token]
			if !ok {
				return nil, fmt.Errorf("JSON pointer %q is missing key %q", pointer, token)
			}
			current = next
		case []any:
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(value) {
				return nil, fmt.Errorf("JSON pointer %q has invalid array index %q", pointer, token)
			}
			current = value[index]
		default:
			return nil, fmt.Errorf("JSON pointer %q traverses a non-container", pointer)
		}
	}
	return current, nil
}

func (c *checker) compileSchemas() (map[string]*jsonschema.Schema, error) {
	paths, err := filepath.Glob(filepath.Join(c.protocolDir, "schemas", "*.schema.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)

	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(jsonschema.FileLoader{})
	compiler.AssertFormat()

	compiled := make(map[string]*jsonschema.Schema, len(paths))
	for _, path := range paths {
		schema, err := compiler.Compile(path)
		if err != nil {
			return nil, fmt.Errorf("compile %s: %w", c.rel(path), err)
		}
		compiled[filepath.Base(path)] = schema
	}
	c.schemaCount = len(compiled)
	return compiled, nil
}

func (c *checker) checkFixtures(schemas map[string]*jsonschema.Schema) error {
	paths, err := filepath.Glob(filepath.Join(c.protocolDir, "fixtures", "*.json"))
	if err != nil {
		return err
	}
	sort.Strings(paths)

	seen := make(map[string]bool, len(paths))
	ticketStatesSeen := make(map[string]bool)
	checkedCount := 0
	var problems []string
	for _, path := range paths {
		name := filepath.Base(path)
		seen[name] = true
		if name == "media-samples.json" {
			continue
		}
		checkedCount++

		schemaName, err := fixtureSchemaName(name)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		schema, ok := schemas[schemaName]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: schema %s was not compiled", name, schemaName))
			continue
		}

		fixture, err := readJSON(path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if err := schema.Validate(fixture); err != nil {
			problems = append(problems, fmt.Sprintf("%s: schema validation failed: %v", name, err))
			continue
		}
		if err := c.checkFixtureSemantics(name, fixture); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
		}
		if expectation, ok := expectedTickets[name]; ok {
			ticketStatesSeen[expectation.state] = true
		}
		if err := c.scanFixtureSafety(name, fixture); err != nil {
			problems = append(problems, err.Error())
		}
	}
	for _, name := range requiredFixtures {
		if !seen[name] {
			problems = append(problems, name+": required fixture is missing")
		}
	}
	for _, state := range requiredTicketStates {
		if !ticketStatesSeen[state] {
			problems = append(problems, fmt.Sprintf("playback ticket fixture coverage is missing state %q", state))
		}
	}
	c.fixtureCount = checkedCount
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

func fixtureSchemaName(name string) (string, error) {
	switch {
	case name == "meta.json":
		return "meta.schema.json", nil
	case name == "device-auth-start.json":
		return "device-authorization-start.schema.json", nil
	case name == "device-auth-pending.json" || name == "device-auth-authorized.json":
		return "device-authorization-status.schema.json", nil
	case name == "device-auth-lookup.json":
		return "device-authorization-lookup.schema.json", nil
	case name == "device-auth-decision.json":
		return "device-authorization-decision.schema.json", nil
	case name == "device-auth-token-pending.json":
		return "device-token-pending.schema.json", nil
	case name == "device-auth-token.json" || name == "device-auth-refresh.json":
		return "auth-token-pair.schema.json", nil
	case name == "device-list.json":
		return "device-list.schema.json", nil
	case name == "device-revoked.json":
		return "device-revocation.schema.json", nil
	case name == "ticket-create-request.json":
		return "playback-ticket-create-request.schema.json", nil
	case name == "media-session.json":
		return "media-session.schema.json", nil
	case name == "subtitle-vtt.json" || name == "subtitle-pgs.json":
		return "subtitle.schema.json", nil
	case strings.HasPrefix(name, "ticket-"):
		expectation, ok := expectedTickets[name]
		if !ok {
			return "", fmt.Errorf("%s: ticket fixture is not declared in expectedTickets", name)
		}
		if expectation.schema == "issuance" {
			return "playback-ticket-issuance.schema.json", nil
		}
		if expectation.schema == "status" {
			return "playback-ticket.schema.json", nil
		}
		return "", fmt.Errorf("%s: ticket fixture has invalid schema class %q", name, expectation.schema)
	case strings.HasPrefix(name, "error-"):
		return "error.schema.json", nil
	default:
		return "", fmt.Errorf("%s: fixture name is not mapped to an S1, S2, or S3 schema", name)
	}
}

func (c *checker) checkFixtureSemantics(name string, value any) error {
	switch {
	case name == "meta.json":
		return checkMetaFixture(value)
	case strings.HasPrefix(name, "device-auth-"), name == "device-list.json", name == "device-revoked.json":
		return checkDeviceAuthFixture(name, value)
	case name == "ticket-create-request.json", name == "media-session.json":
		return nil
	case name == "subtitle-vtt.json" || name == "subtitle-pgs.json":
		return checkSubtitleFixture(name, value)
	case strings.HasPrefix(name, "ticket-"):
		return checkTicketFixture(name, value)
	case strings.HasPrefix(name, "error-"):
		return checkErrorFixture(name, value)
	default:
		return nil
	}
}

func checkDeviceAuthFixture(name string, value any) error {
	want, ok := expectedAuthorizationStates[name]
	if !ok {
		return nil
	}
	doc, err := asObject(value, "$")
	if err != nil {
		return err
	}
	state, err := asString(doc["state"], "$.state")
	if err != nil {
		return err
	}
	if state != want {
		return fmt.Errorf("state must be %q, got %q", want, state)
	}
	if name == "device-auth-lookup.json" {
		csrfToken, err := asString(doc["csrf_token"], "$.csrf_token")
		if err != nil {
			return fmt.Errorf("pending lookup fixture must include csrf_token: %w", err)
		}
		if !isExampleCredential(csrfToken) {
			return fmt.Errorf("$.csrf_token must use an obvious example placeholder")
		}
	}
	return nil
}

func checkMetaFixture(value any) error {
	doc, err := asObject(value, "$")
	if err != nil {
		return err
	}
	if serverVersion, _ := doc["server_version"].(string); serverVersion != "dev" {
		return fmt.Errorf("server_version must be %q", "dev")
	}
	features, err := asObject(doc["features"], "$.features")
	if err != nil {
		return err
	}
	for flag, value := range features {
		enabled, ok := value.(bool)
		if !ok {
			return fmt.Errorf("feature %s must be boolean", flag)
		}
		if enabled {
			return fmt.Errorf("fixture feature %s must default to false", flag)
		}
	}
	return nil
}

func checkSubtitleFixture(name string, value any) error {
	doc, err := asObject(value, "$")
	if err != nil {
		return err
	}
	format, err := asString(doc["format"], "$.format")
	if err != nil {
		return err
	}
	rawURL, hasURL := doc["url"]
	switch name {
	case "subtitle-vtt.json":
		if format != "vtt" || !hasURL {
			return fmt.Errorf("WebVTT fixture must use format vtt and include url")
		}
	case "subtitle-pgs.json":
		if format != "pgs" || hasURL {
			return fmt.Errorf("PGS fixture must be descriptive and omit url")
		}
	}
	if hasURL {
		subtitleURL, err := asString(rawURL, "$.url")
		if err != nil {
			return err
		}
		parsed, err := url.Parse(subtitleURL)
		if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("$.url must not contain a query or fragment")
		}
		if !strings.Contains(parsed.Path, "/api/v1/playback/") {
			return fmt.Errorf("$.url must use a controlled playback path")
		}
	}
	return nil
}

func checkTicketFixture(name string, value any) error {
	expectation, ok := expectedTickets[name]
	if !ok {
		return fmt.Errorf("ticket fixture is not declared in expectedTickets")
	}
	doc, err := asObject(value, "$")
	if err != nil {
		return err
	}
	state, err := asString(doc["state"], "$.state")
	if err != nil {
		return err
	}
	if state != expectation.state {
		return fmt.Errorf("state must be %q, got %q", expectation.state, state)
	}
	if expectation.failureCode != "" {
		failureCode, err := asString(doc["failure_code"], "$.failure_code")
		if err != nil {
			return err
		}
		if failureCode != expectation.failureCode {
			return fmt.Errorf("failure_code must be %q, got %q",
				expectation.failureCode, failureCode)
		}
	}
	if expectation.schema == "status" {
		forbidden := map[string]bool{
			"headers":           true,
			"media_session":     true,
			"source_expires_at": true,
			"streams":           true,
			"subtitles":         true,
			"transport":         true,
			"url":               true,
			"capability":        true,
		}
		if keyPath := findForbiddenJSONKey(value, "$", forbidden); keyPath != "" {
			return fmt.Errorf("safe GET status fixture must not contain issuance field %s", keyPath)
		}
	}
	if expectation.kind == "" {
		return nil
	}
	transport, err := asObject(doc["transport"], "$.transport")
	if err != nil {
		return err
	}
	kind, err := asString(transport["kind"], "$.transport.kind")
	if err != nil {
		return err
	}
	if kind != expectation.kind {
		return fmt.Errorf("transport.kind must be %q, got %q", expectation.kind, kind)
	}
	if rawSubtitles, exists := transport["subtitles"]; exists {
		subtitles, err := asArray(rawSubtitles, "$.transport.subtitles")
		if err != nil {
			return err
		}
		seen := make(map[string]bool, len(subtitles))
		for i, rawSubtitle := range subtitles {
			subtitle, err := asObject(
				rawSubtitle, fmt.Sprintf("$.transport.subtitles[%d]", i))
			if err != nil {
				return err
			}
			id, err := asString(
				subtitle["id"], fmt.Sprintf("$.transport.subtitles[%d].id", i))
			if err != nil {
				return err
			}
			if seen[id] {
				return fmt.Errorf("$.transport.subtitles contains duplicate id %q", id)
			}
			seen[id] = true
			format, err := asString(
				subtitle["format"], fmt.Sprintf("$.transport.subtitles[%d].format", i))
			if err != nil {
				return err
			}
			_, hasURL := subtitle["url"]
			if format == "pgs" && hasURL {
				return fmt.Errorf("$.transport.subtitles[%d] PGS entry must omit url", i)
			}
			if kind == "server-hls" && format != "vtt" {
				return fmt.Errorf("server-HLS subtitle %d must be WebVTT", i)
			}
		}
	}
	remaining, err := asArray(doc["remaining_transports"], "$.remaining_transports")
	if err != nil {
		return err
	}
	for i, raw := range remaining {
		value, err := asString(raw, fmt.Sprintf("$.remaining_transports[%d]", i))
		if err != nil {
			return err
		}
		if value == kind {
			return fmt.Errorf("remaining_transports must not repeat the issued transport %q", kind)
		}
	}
	return nil
}

func findForbiddenJSONKey(value any, path string, forbidden map[string]bool) string {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			normalized := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
			if forbidden[normalized] {
				return path + "." + key
			}
			if found := findForbiddenJSONKey(typed[key], path+"."+key, forbidden); found != "" {
				return found
			}
		}
	case []any:
		for i, item := range typed {
			if found := findForbiddenJSONKey(item, fmt.Sprintf("%s[%d]", path, i), forbidden); found != "" {
				return found
			}
		}
	}
	return ""
}

func checkErrorFixture(name string, value any) error {
	want, ok := expectedErrors[name]
	if !ok {
		return nil
	}
	doc, err := asObject(value, "$")
	if err != nil {
		return err
	}
	errorObject, err := asObject(doc["error"], "$.error")
	if err != nil {
		return err
	}
	code, err := asString(errorObject["code"], "$.error.code")
	if err != nil {
		return err
	}
	if code != want {
		return fmt.Errorf("error.code must be %q, got %q", want, code)
	}
	switch name {
	case "error-csrf-invalid.json":
		details, err := requiredObject(errorObject, "details", "$.error")
		if err != nil {
			return err
		}
		action, err := asString(details["action"], "$.error.details.action")
		if err != nil || action != "reload_authorization" {
			return fmt.Errorf("$.error.details.action must be reload_authorization")
		}
	case "error-email-verification-required.json":
		if _, ok := errorObject["details"]; ok {
			return fmt.Errorf("email verification gate response must not invent error.details")
		}
	}
	return nil
}

func (c *checker) scanFixtureSafety(name string, value any) error {
	var problems []string
	seenStrings := make(map[string]bool)

	var walk func(path string, current any)
	walk = func(path string, current any) {
		switch v := current.(type) {
		case map[string]any:
			keys := make([]string, 0, len(v))
			for key := range v {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				childPath := path + "." + key
				if isSensitiveKey(key) {
					problems = append(problems, fmt.Sprintf("%s: sensitive field %s", name, childPath))
				}
				if isCredentialValueKey(key) {
					if credential, ok := v[key].(string); ok && !isExampleCredential(credential) {
						problems = append(problems, fmt.Sprintf("%s: %s must use an obvious example placeholder", name, childPath))
					}
				}
				walk(childPath, v[key])
			}
		case []any:
			for i, item := range v {
				walk(fmt.Sprintf("%s[%d]", path, i), item)
			}
		case string:
			if seenStrings[v] {
				return
			}
			seenStrings[v] = true
			for _, problem := range checkFixtureString(v, path) {
				problems = append(problems, fmt.Sprintf("%s: %s at %s", name, problem, path))
			}
		}
	}

	walk("$", value)
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

func isSensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	switch normalized {
	case "authorization_id",
		"device_code",
		"user_code",
		"code_challenge",
		"code_verifier",
		"access_token",
		"refresh_token",
		"csrf_token",
		"refresh_expires_in",
		"token_type":
		return false
	}
	if normalized == "sig" || strings.HasSuffix(normalized, "_sig") {
		return true
	}
	for _, token := range []string{
		"authorization",
		"cookie",
		"token",
		"refresh",
		"access",
		"basic",
		"password",
		"secret",
		"signature",
		"credential",
		"bearer",
		"api_key",
		"apikey",
		"private_key",
		"hash",
	} {
		if strings.Contains(normalized, token) {
			return true
		}
	}
	for _, token := range []string{
		"ip",
		"remote_addr",
		"client_addr",
	} {
		if normalized == token || strings.HasSuffix(normalized, "_"+token) {
			return true
		}
	}
	return false
}

func isCredentialValueKey(key string) bool {
	switch strings.ToLower(strings.ReplaceAll(key, "-", "_")) {
	case "device_code", "access_token", "refresh_token", "csrf_token", "code_verifier":
		return true
	default:
		return false
	}
}

func isExampleCredential(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "example") && strings.Contains(lower, "not_a_real")
}

func checkFixtureString(value, path string) []string {
	var problems []string
	if bearerValue.MatchString(value) {
		problems = append(problems, "bearer credential-like value is forbidden")
	}
	if basicValue.MatchString(value) {
		problems = append(problems, "basic credential-like value is forbidden")
	}
	if jwtValue.MatchString(value) {
		problems = append(problems, "JWT-like value is forbidden")
	}
	if secretQuery.MatchString(value) {
		problems = append(problems, "query signature-like parameter is forbidden")
	}

	if !strings.Contains(value, "://") {
		return problems
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return append(problems, "URL cannot be parsed")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return append(problems, "fixture URLs must use http or https")
	}
	if parsed.User != nil {
		return append(problems, "URL userinfo is forbidden")
	}
	if parsed.RawQuery != "" {
		query := parsed.Query()
		values, hasCode := query["code"]
		completeURL := path == "$.verification_uri_complete"
		if !completeURL || len(query) != 1 || !hasCode || len(values) != 1 || !fixtureUserCode.MatchString(values[0]) {
			problems = append(problems, "fixture URLs may only use a valid code query on verification_uri_complete")
		}
	}
	host := parsed.Hostname()
	if !isFixtureHost(host) {
		problems = append(problems, fmt.Sprintf("URL host %q is not an approved fixture host", host))
	}
	return problems
}

func isFixtureHost(host string) bool {
	if host == "localhost" || host == "example.com" || strings.HasSuffix(host, ".example.com") || strings.HasSuffix(host, ".example") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func readYAML(path string) (any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: parse YAML: %w", path, err)
	}
	return normalizeYAML(doc), nil
}

func normalizeYAML(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, child := range v {
			out[key] = normalizeYAML(child)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(v))
		for key, child := range v {
			out[fmt.Sprint(key)] = normalizeYAML(child)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = normalizeYAML(child)
		}
		return out
	default:
		return value
	}
}

func readJSON(path string) (any, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	value, err := jsonschema.UnmarshalJSON(file)
	if err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	return value, nil
}

func asObject(value any, path string) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", path)
	}
	return object, nil
}

func asArray(value any, path string) ([]any, error) {
	array, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array", path)
	}
	return array, nil
}

func requiredObject(parent map[string]any, key, path string) (map[string]any, error) {
	value, ok := parent[key]
	if !ok {
		return nil, fmt.Errorf("%s is missing %s", path, key)
	}
	return asObject(value, path+"."+key)
}

func asString(value any, path string) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", path)
	}
	return text, nil
}

func asBool(value any, path string) (bool, error) {
	boolean, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("%s must be a boolean", path)
	}
	return boolean, nil
}

func stringArray(value any, path string) ([]string, error) {
	raw, err := asArray(value, path)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(raw))
	for i, item := range raw {
		text, err := asString(item, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return nil, err
		}
		out = append(out, text)
	}
	return out, nil
}

func asInteger(value any, path string) (int64, error) {
	switch number := value.(type) {
	case int:
		return int64(number), nil
	case int64:
		return number, nil
	case uint64:
		return int64(number), nil
	case json.Number:
		parsed, err := strconv.ParseInt(number.String(), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer", path)
		}
		return parsed, nil
	case float64:
		if number != float64(int64(number)) {
			return 0, fmt.Errorf("%s must be an integer", path)
		}
		return int64(number), nil
	default:
		return 0, fmt.Errorf("%s must be an integer", path)
	}
}

func (c *checker) rel(path string) string {
	relative, err := filepath.Rel(c.root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(relative)
}
