package fault

import (
	"fmt"
	"strings"
	"testing"
)

func TestOnlyExplicitPublicErrorsAreDisplayed(t *testing.T) {
	private := fmt.Errorf("parser detail PRIVATE_SECRET")
	if strings.Contains(Message(private), "PRIVATE_SECRET") {
		t.Fatal("private error exposed")
	}
	public := fmt.Errorf("internal wrapper: %w", New("selected session is absent"))
	if Message(public) != "selected session is absent" {
		t.Fatal(Message(public))
	}
}
