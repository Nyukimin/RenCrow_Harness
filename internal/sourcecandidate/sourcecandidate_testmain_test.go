package sourcecandidate

import (
	"fmt"
	"os"
	"testing"

	"github.com/Nyukimin/RenCrow_Harness/internal/fsperm"
)

func TestMain(m *testing.M) {
	if err := fsperm.InitializeProcessOwner(); err != nil {
		fmt.Fprintln(os.Stderr, "source candidate test process owner could not be initialized")
		os.Exit(1)
	}
	os.Exit(m.Run())
}
