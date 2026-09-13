package modem

import "testing"

func TestResolveServingOperatorNameFromPLMN(t *testing.T) {
	tests := []struct {
		name string
		code string
		want string
	}{
		{name: "china telecom plmn", code: "46011", want: "中国电信"},
		{name: "china unicom plmn", code: "46001", want: "中国联通"},
		{name: "quoted code", code: "\"46015\"", want: "中国广电"},
		{name: "unknown code fallback", code: "99999", want: "99999"},
		{name: "empty code", code: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveServingOperatorNameFromPLMN(tt.code)
			if got != tt.want {
				t.Fatalf("ResolveServingOperatorNameFromPLMN(%q)=%q want=%q", tt.code, got, tt.want)
			}
		})
	}
}

func TestServingRegistrationCurrent(t *testing.T) {
	if ServingRegistrationCurrent(0) || ServingRegistrationCurrent(2) || ServingRegistrationCurrent(3) || ServingRegistrationCurrent(4) {
		t.Fatal("unregistered/searching must not count as a live camp")
	}
	if !ServingRegistrationCurrent(1) || !ServingRegistrationCurrent(5) {
		t.Fatal("home and roaming must count as a live camp")
	}
}

func TestLookupServingOperatorNameFromPLMN(t *testing.T) {
	tests := []struct {
		name string
		code string
		want string
		ok   bool
	}{
		{name: "known", code: "46000", want: "中国移动", ok: true},
		{name: "unknown", code: "310260", want: "", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := LookupServingOperatorNameFromPLMN(tt.code)
			if ok != tt.ok {
				t.Fatalf("LookupServingOperatorNameFromPLMN(%q) ok=%v want=%v", tt.code, ok, tt.ok)
			}
			if got != tt.want {
				t.Fatalf("LookupServingOperatorNameFromPLMN(%q)=%q want=%q", tt.code, got, tt.want)
			}
		})
	}
}
