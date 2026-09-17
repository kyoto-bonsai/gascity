package api

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestSessionWokePayloadOmitsSecrets (ga-dbfydw) mirrors
// TestBackendCredentialResolvedPayloadOmitsTheCredential's method: the field
// set is closed, every field is a plain string identifier, and a populated
// payload marshals to exactly those keys -- so an env map (which mixes real
// provider credentials into GC_*-prefixed identity vars, see
// internal/runtime/envsecret.go) cannot arrive through an embedded type or a
// custom marshaller either. A new field widening this set is a deliberate
// edit that must come back through here.
func TestSessionWokePayloadOmitsSecrets(t *testing.T) {
	want := []string{"model", "model_source", "provider"}

	typ := reflect.TypeOf(SessionWokePayload{})
	got := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		field := typ.Field(i)
		tag, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if tag == "" {
			t.Fatalf("field %s has no json tag: an untagged field crosses the wire under its Go name", field.Name)
		}
		got = append(got, tag)
		if field.Type.Kind() != reflect.String {
			t.Errorf("field %s is %s: every field here is a plain identifier, never a map or nested struct "+
				"that could carry an env value along for the ride", field.Name, field.Type)
		}
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("payload fields = %v, want %v", got, want)
	}

	canary := "session-woke-canary"
	encoded, err := json.Marshal(SessionWokePayload{
		Provider:    canary,
		Model:       canary,
		ModelSource: "default", // a bare literal: the vocabulary lives with the
		// resolver in cmd/gc (session_model_resolution.go), not this wire type --
		// this field just carries whatever string the resolver produced.
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	keys := make([]string, 0, len(decoded))
	for key := range decoded {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("payload JSON keys = %v, want %v", keys, want)
	}
	for _, forbidden := range []string{"env", "environment", "secret", "token", "credential", "password", "api_key", "auth"} {
		if _, ok := decoded[forbidden]; ok {
			t.Errorf("payload JSON carries %q: %s", forbidden, encoded)
		}
	}
}

// TestSessionWokePayloadAllFieldsOmitemptyWhenUnresolved (ga-dbfydw) pins the
// zero-value shape: a session.woke for a provider whose schema has no model
// concept (ResolvedModel/ResolvedModelSource never set) must not print a
// misleading empty string for model/model_source -- the fields disappear
// entirely, so a consumer can distinguish "no model concept for this
// provider" from "the model happens to be the empty string" (the latter
// never legitimately occurs, but omitempty is the honest contract either
// way, matching this codebase's stated preference elsewhere for omitting an
// unavailable field over emitting a zero value that reads as a fact).
func TestSessionWokePayloadAllFieldsOmitemptyWhenUnresolved(t *testing.T) {
	encoded, err := json.Marshal(SessionWokePayload{Provider: "claude"})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if strings.Contains(string(encoded), "model") {
		t.Fatalf("payload = %s, want no model/model_source key at all when unset", encoded)
	}
}
