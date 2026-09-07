package plugin

import (
	"os"
	"testing"

	"github.com/torana-edge/torana-edge/internal/testfixture"
)

func TestMain(m *testing.M) {
	testfixture.WarnIfUnbuilt("../../examples/plugins")
	os.Exit(m.Run())
}
