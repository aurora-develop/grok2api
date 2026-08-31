package tlsclient

import "testing"

func TestClientExposesBrowserDialers(t *testing.T) {
	client := New()
	if client.GetDialer() == nil {
		t.Fatal("browser dialer is nil")
	}
	if client.GetTLSDialer() == nil {
		t.Fatal("browser TLS dialer is nil")
	}
}
