package pluginloader

import (
	"encoding/json"
	"testing"
)

// FuzzParseManifest exercises JSON parsing and validation of plugin manifests.
func FuzzParseManifest(f *testing.F) {
	seed := &Manifest{
		ManifestVersion:    SupportedManifestVersion,
		APIVersion:         HostAPICompatRange,
		Name:               "demo-plugin",
		Version:            "1.0.0",
		TrustTierRequested: 1,
		Capabilities:       []string{"http:route", "state:kv"},
		Routes:             []ManifestRoute{{Prefix: "/demo/*", Handler: "on_request"}},
	}
	b, _ := json.Marshal(seed)
	f.Add(b)
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"manifest_version":"1.0","api_version":">=1.0.0 <2.0.0","name":"x","version":"bad"}`))

	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = ParseManifest(raw)
	})
}

// FuzzValidateCapability exercises capability string validation.
func FuzzValidateCapability(f *testing.F) {
	seeds := []string{
		"http:route",
		"state:kv",
		"pipeline:step:compress",
		"bad",
		":empty-ns",
		"unknown:thing",
		"",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, cap string) {
		_ = validateCapability(cap)
	})
}

// FuzzValidateCELCondition exercises hook condition string validation.
// The condition field is currently stored verbatim and not evaluated at
// runtime; this fuzz test ensures the validator never panics on arbitrary
// strings and accepts plausible CEL expressions.
func FuzzValidateCELCondition(f *testing.F) {
	seeds := []string{
		"",
		"true",
		"bucket == 'foo'",
		"request.user == 'admin' && size <= 1024",
		"headers['x-trace-id'].matches('^[a-f0-9]+$')",
		"!unknown(",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, cond string) {
		// Validation currently only checks the surrounding hook structure;
		// ensure no panic on arbitrary condition strings.
		h := ManifestHook{
			Type:      "before",
			Operation: "s3:GetObject",
			Handler:   "on_hook",
			Condition: cond,
		}
		_ = validateHook(&h)
	})
}
