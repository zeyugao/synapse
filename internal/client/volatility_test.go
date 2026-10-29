package client

import (
	"fmt"
	"testing"

	"github.com/zeyugao/synapse/internal/types"
)

func sample(t *testing.T, raw string) []*types.ModelInfo {
	t.Helper()
	m, err := decodeModelInfo([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return []*types.ModelInfo{m}
}

func TestActiveVolatilityDetection(t *testing.T) {
	payload := func(created int, permission string, limit int) []*types.ModelInfo {
		return sample(t, fmt.Sprintf(`{"id":"m","created":%d,"permission":[{"id":%q,"created":%d}],"max_model_len":%d}`, created, permission, created, limit))
	}
	c := &Client{models: payload(1, "p1", 262144)}
	for i := 2; i < 5; i++ {
		if c.metadataChanged(payload(i, fmt.Sprint("p", i), 262144)) {
			t.Fatal("volatile timestamp or permission ID triggered update")
		}
	}
	next := payload(5, "p5", 1048576)
	if c.metadataChanged(next) {
		t.Fatal("one-off change should wait until stable")
	}
	latest := payload(6, "p6", 1048576)
	if !c.metadataChanged(latest) {
		t.Fatal("stable context change was not detected")
	}
	// Simulate syncModels advertising the complete current payload.
	c.models = latest
	if c.metadataChanged(payload(7, "p7", 1048576)) {
		t.Fatal("continuous churn triggered another update")
	}
	// Previously unstable paths are not permanently blacklisted.
	if !c.metadataChanged(payload(7, "p7", 1048576)) {
		t.Fatal("settled volatile fields should propagate")
	}
}

func TestStableMetadataShapesAndValues(t *testing.T) {
	for _, tc := range []struct{ name, old, next string }{
		{"addition", `{"id":"m"}`, `{"id":"m","root":null}`},
		{"removal", `{"id":"m","root":null}`, `{"id":"m"}`},
		{"null to empty array", `{"id":"m","x":null}`, `{"id":"m","x":[]}`},
		{"empty array to object", `{"id":"m","x":[]}`, `{"id":"m","x":{}}`},
		{"array length", `{"id":"m","x":[null]}`, `{"id":"m","x":[null,null]}`},
		{"nested array leaf", `{"id":"m","x":[{"a":[1,2]}]}`, `{"id":"m","x":[{"a":[1,3]}]}`},
		{"large integers", `{"id":"m","x":9007199254740992}`, `{"id":"m","x":9007199254740993}`},
		{"escaped keys", `{"id":"m","a/b":{"~":1}}`, `{"id":"m","a/b":{"~":2}}`},
		{"typed field", `{"id":"m","created":1}`, `{"id":"m","created":2}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{models: sample(t, tc.old)}
			next := sample(t, tc.next)
			if c.metadataChanged(next) {
				t.Fatal("not yet stable")
			}
			if !c.metadataChanged(next) {
				t.Fatal("settled change missing")
			}
			c.models = next
			if c.metadataChanged(next) {
				t.Fatal("duplicate notification")
			}
		})
	}
}

func TestVolatilityResetOnModelIDsAndEmptyCatalog(t *testing.T) {
	c := &Client{models: sample(t, `{"id":"m","created":1}`)}
	c.metadataChanged(sample(t, `{"id":"m","created":2}`))
	next := sample(t, `{"id":"new","created":3}`)
	if !c.metadataChanged(next) {
		t.Fatal("new ID must propagate immediately")
	}
	if len(c.suppressedMetadata) != 0 {
		t.Fatal("suppression state not reset")
	}
	c.models = next
	if c.metadataChanged(sample(t, `{"id":"new","created":4}`)) {
		t.Fatal("new ID inherited obsolete baseline")
	}
	if !c.metadataChanged(nil) {
		t.Fatal("removal must propagate immediately")
	}
	c.models = nil
	if c.metadataChanged(nil) {
		t.Fatal("empty catalog churn")
	}
}

func TestVolatilityIgnoresObjectAndModelOrdering(t *testing.T) {
	a := sample(t, `{"id":"a","root":"x","flags":{"a":true,"b":false}}`)
	b := sample(t, `{"id":"b","root":"y"}`)
	c := &Client{models: append(a, b...)}
	reordered := append(b, sample(t, `{"flags":{"b":false,"a":true},"root":"x","id":"a"}`)...)
	if c.metadataChanged(reordered) {
		t.Fatal("ordering triggered update")
	}
	if c.metadataChanged(reordered) {
		t.Fatal("ordering triggered delayed update")
	}
}
