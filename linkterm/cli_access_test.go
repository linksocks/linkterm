package linkterm

import "testing"

func TestNewLinkTermDialAccessControl(t *testing.T) {
	ac, err := newLinkTermDialAccessControl(8080)
	if err != nil {
		t.Fatalf("newLinkTermDialAccessControl() error = %v", err)
	}
	if !ac.Allow("127.0.0.1", 8080) {
		t.Fatal("LinkTerm server port should be allowed")
	}
	if ac.Allow("127.0.0.1", 22) {
		t.Fatal("other ports should be blocked")
	}
}
