package access_control

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
)

// The same policy is evaluated for many subjects during membership queries.
func BenchmarkEngineRepeatedPolicy(b *testing.B) {
	engine, err := newEngine()
	if err != nil {
		b.Fatal(err)
	}
	subject := model.Subject{Attributes: map[string]any{"department": "Engineering", "programs": []string{"Orion"}}}
	expression := `user.attributes.department == "Engineering" && user.attributes.programs.hasAnyOf(["Orion", "Artemis"])`
	if allowed, err := engine.evaluate(expression, subject, nil); err != nil || !allowed {
		b.Fatalf("policy evaluation: allowed=%v err=%v", allowed, err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if allowed, err := engine.evaluate(expression, subject, nil); err != nil || !allowed {
			b.Fatalf("policy evaluation: allowed=%v err=%v", allowed, err)
		}
	}
}
