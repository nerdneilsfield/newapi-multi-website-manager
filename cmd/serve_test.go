package cmd

import (
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"github.com/nerdneilsfield/newapi-multi-website-manager/internal/store"
)

func TestBootstrapAdminRejectsShortPassword(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	t.Setenv("NAPM_ADMIN_PASSWORD", "1234567")
	if err := bootstrapAdmin(st, &cobra.Command{}); err == nil {
		t.Fatal("7-char password accepted")
	}
	if n, _ := st.CountAdmins(); n != 0 {
		t.Fatalf("admin created: %d", n)
	}
	t.Setenv("NAPM_ADMIN_PASSWORD", "12345678")
	if err := bootstrapAdmin(st, &cobra.Command{}); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountAdmins(); n != 1 {
		t.Fatalf("admins: %d", n)
	}
}
