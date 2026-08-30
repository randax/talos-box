package main

import (
	"errors"
	"testing"
)

func TestBalloonDisabledFindingOnlyWhenDisabled(t *testing.T) {
	if _, ok := balloonDisabledFinding(nil); ok {
		t.Fatal("nil probe produced a finding")
	}
	if _, ok := balloonDisabledFinding(func() (bool, error) { return false, nil }); ok {
		t.Fatal("enabled balloon produced a finding")
	}
	if _, ok := balloonDisabledFinding(func() (bool, error) { return true, errors.New("daemon down") }); ok {
		t.Fatal("unreachable daemon produced a finding")
	}
	finding, ok := balloonDisabledFinding(func() (bool, error) { return true, nil })
	if !ok || finding.level != "INFO" || finding.check != "balloon" {
		t.Fatalf("disabled balloon finding = %+v, %t; want INFO balloon", finding, ok)
	}
}
