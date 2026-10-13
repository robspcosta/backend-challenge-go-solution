package app

import (
	"go.uber.org/fx"
	"testing"
)

func TestComposition(t *testing.T) {
	if err := fx.ValidateApp(Options()); err != nil {
		t.Fatal(err)
	}
}
