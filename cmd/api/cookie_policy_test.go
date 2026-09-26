package main

import "testing"

func TestSessionCookiesSecure(t *testing.T) {
	tests := []struct {
		name     string
		layer    string
		origin   string
		override string
		want     bool
		wantErr  bool
	}{
		{"production default", "prod", "https://example.com", "", true, false},
		{"http still secure by default", "prod", "http://example.com", "", true, false},
		{"explicit http trial", "prod", "http://example.com", "true", false, false},
		{"https override rejected", "prod", "https://example.com", "true", false, true},
		{"dev override rejected", "dev", "http://example.com", "true", false, true},
		{"invalid override rejected", "prod", "http://example.com", "yes", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sessionCookiesSecure(tt.layer, tt.origin, tt.override)
			if (err != nil) != tt.wantErr || (!tt.wantErr && got != tt.want) {
				t.Fatalf("sessionCookiesSecure() = %v, %v; want %v, error=%v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}
