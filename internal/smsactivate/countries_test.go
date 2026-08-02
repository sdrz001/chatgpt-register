package smsactivate

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestCountryCatalogAndValidation(t *testing.T) {
	countries := Countries()
	if len(countries) < 190 {
		t.Fatalf("countries=%d", len(countries))
	}
	countries[0].Label = "changed"
	if Countries()[0].Label == "changed" {
		t.Fatal("Countries returned shared storage")
	}
	config := validConfig()
	config.Country = 12
	if err := config.Validate(); !IsCode(err, CodeConfig) {
		t.Fatalf("unsupported fixed country error=%v", err)
	}
	config.Country = 0
	config.RandomCountries = []int{16, 12}
	if err := config.Validate(); !IsCode(err, CodeConfig) {
		t.Fatalf("unsupported random country error=%v", err)
	}
}

func TestUnexpectedResponseRedactsAPIKey(t *testing.T) {
	client, server := newTestClient(t, validConfig(), func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "unexpected response containing %s", testAPIKey)
	})
	defer server.Close()
	_, err := client.GetBalance(context.Background())
	if err == nil || strings.Contains(err.Error(), testAPIKey) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("error=%v", err)
	}
}
