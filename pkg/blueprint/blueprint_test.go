package blueprint

import (
	"encoding/json"
	"testing"
)

func TestBlueprintMarshalRoundTrip(t *testing.T) {
	bp := &Blueprint{
		Version:   1,
		Name:      "test",
		PortPool:  PortPool{Start: 3000, End: 4000},
		Toolchain: ".tool-versions",
		Seed:      SeedConfig{Command: "bun run db fixtures", Workdir: "."},
		Services: map[string]ServiceDef{
			"web": {
				Isolation: IsolationDedicated,
				Image:     "nginx:latest",
				Ports:     map[string]PortDef{"80": {Var: "PORT"}},
			},
		},
		Processes: []ProcessDef{
			{Name: "app", Isolation: "native", Command: "next dev", Workdir: ".", PortVar: "PORT", DefaultPort: 3000},
		},
		Env: EnvConfig{
			Template: ".env.example",
			Holes:    map[string]string{"KEY": "value"},
		},
	}

	data, err := json.Marshal(bp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Blueprint
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Version != 1 || got.Name != "test" {
		t.Errorf("round trip lost fields: got version=%d name=%s", got.Version, got.Name)
	}
}

func TestBlueprintUnmarshal_UnknownFields(t *testing.T) {
	input := `{"version":1,"name":"x","port_pool":{"start":3000,"end":4000},"seed":{"command":"x","workdir":"."},"env":{"template":"x","holes":{}},"unknown_field":"should_not_cause_error"}`
	var bp Blueprint
	if err := json.Unmarshal([]byte(input), &bp); err != nil {
		t.Fatalf("unmarshal with unknown field should not error: %v", err)
	}
}
