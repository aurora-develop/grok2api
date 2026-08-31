package grok

import (
	"strings"
	"testing"
)

func TestGatewayCookieReplacesExistingUserID(t *testing.T) {
	profile := proxyProfile{CFCookies: "cf_clearance=test; x-userid=old-user"}
	cookie := buildGatewayCookie("token", profile, "new-user")
	if strings.Count(cookie, "x-userid=") != 1 || !strings.Contains(cookie, "x-userid=new-user") {
		t.Fatalf("cookie = %q", cookie)
	}
}
