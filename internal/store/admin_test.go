package store

import (
	"os"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestVerifyAdminCachesAndInvalidatesOnPasswordChange(t *testing.T) {
	dir, _ := os.MkdirTemp("", "cfrproxy-admin")
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	set := func(pass string) {
		h, _ := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.MinCost)
		s.SetSetting("admin_user", "admin")
		s.SetSetting("admin_pass_hash", string(h))
	}
	set("one")
	if !s.VerifyAdmin("admin", "one") || s.VerifyAdmin("admin", "two") || s.VerifyAdmin("root", "one") {
		t.Fatal("basic verification wrong")
	}
	if n := len(s.admin.ok); n != 1 {
		t.Fatalf("expected one cached credential, have %d", n)
	}
	set("two")
	if s.VerifyAdmin("admin", "one") {
		t.Fatal("old password still accepted after the hash changed")
	}
	if !s.VerifyAdmin("admin", "two") {
		t.Fatal("new password rejected")
	}
}
