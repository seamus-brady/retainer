package paths

import (
	"path/filepath"
	"testing"
)

func TestResolve(t *testing.T) {
	cases := []struct {
		name     string
		explicit string
		env      map[string]string
		want     string
	}{
		{
			name: "default — $HOME/retainer",
			env:  nil,
			want: "/home/u/retainer",
		},
		{
			name: "env var overrides default",
			env:  map[string]string{"RETAINER_WORKSPACE": "/some/env/ws"},
			want: "/some/env/ws",
		},
		{
			name:     "explicit overrides env",
			explicit: "/explicit/ws",
			env:      map[string]string{"RETAINER_WORKSPACE": "/env/ws"},
			want:     "/explicit/ws",
		},
		{
			name:     "explicit overrides default",
			explicit: "/explicit/ws",
			want:     "/explicit/ws",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RETAINER_WORKSPACE", "")
			t.Setenv("HOME", "/home/u")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			got, err := Resolve(tc.explicit)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			wantWS := filepath.FromSlash(tc.want)
			if got.Workspace != wantWS {
				t.Errorf("Workspace = %q, want %q", got.Workspace, wantWS)
			}
			wantCfg := filepath.Join(wantWS, "config")
			if got.Config != wantCfg {
				t.Errorf("Config = %q, want %q", got.Config, wantCfg)
			}
			wantData := filepath.Join(wantWS, "data")
			if got.Data != wantData {
				t.Errorf("Data = %q, want %q", got.Data, wantData)
			}
		})
	}
}
