package ota

import (
	"reflect"
	"testing"
)

func TestEnvVariables(t *testing.T) {
	tests := []struct {
		name string
		data map[string]any
		want map[string]string
		ok   bool
	}{
		{"absent", map[string]any{}, nil, false},
		{"nil", map[string]any{"env_variables": nil}, nil, false},
		{"wrong type", map[string]any{"env_variables": "FOO=bar"}, nil, false},
		{"empty map", map[string]any{"env_variables": map[string]any{}}, nil, false},
		{
			"valid",
			map[string]any{"env_variables": map[string]any{"FOO": "bar", "N": 3}},
			map[string]string{"FOO": "bar", "N": "3"},
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := envVariables(tt.data)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if ok && !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}
