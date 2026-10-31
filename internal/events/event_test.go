package events

import "testing"

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want Event
	}{
		{
			name: "podman shape",
			raw:  `{"status":"health_status","id":"top","Type":"container","Action":"health_status","Actor":{"ID":"abc","Attributes":{"name":"web"}},"HealthStatus":"healthy"}`,
			want: Event{Kind: KindHealth, ContainerID: "abc", Name: "web", Health: "healthy"},
		},
		{
			name: "docker shape",
			raw:  `{"Type":"container","Action":"health_status: unhealthy","Actor":{"ID":"abc"}}`,
			want: Event{Kind: KindHealth, ContainerID: "abc", Health: "unhealthy"},
		},
		{
			name: "attributes fallback",
			raw:  `{"Type":"container","Action":"health_status","Actor":{"ID":"abc","Attributes":{"health_status":"starting"}}}`,
			want: Event{Kind: KindHealth, ContainerID: "abc", Health: "starting"},
		},
		{
			name: "no health value",
			raw:  `{"Type":"container","Action":"health_status","Actor":{"ID":"abc"}}`,
			want: Event{Kind: KindHealth, ContainerID: "abc"},
		},
		{
			name: "status used when Action empty, id fallback",
			raw:  `{"status":"health_status","id":"top","Type":"container","HealthStatus":"healthy"}`,
			want: Event{Kind: KindHealth, ContainerID: "top", Health: "healthy"},
		},
		{
			name: "lifecycle",
			raw:  `{"Type":"container","Action":"die","Actor":{"ID":"abc"}}`,
			want: Event{Kind: KindLifecycle, ContainerID: "abc"},
		},
		{
			name: "other container action",
			raw:  `{"Type":"container","Action":"exec_died","Actor":{"ID":"abc"}}`,
			want: Event{Kind: KindOther, ContainerID: "abc"},
		},
		{
			name: "non-container type",
			raw:  `{"Type":"image","Action":"health_status","Actor":{"ID":"img"},"HealthStatus":"healthy"}`,
			want: Event{Kind: KindOther, ContainerID: "img"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Classify([]byte(tt.raw))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestClassifyMalformed(t *testing.T) {
	for _, raw := range []string{`{"Type":`, `[1,2]`, `{"Actor":{"ID":5}}`} {
		if _, err := Classify([]byte(raw)); err == nil {
			t.Errorf("%s: expected error", raw)
		}
	}
}
