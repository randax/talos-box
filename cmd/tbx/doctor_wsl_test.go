package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/randax/talos-box/internal/wsl"
)

func completeWSLIdentity() wsl.Identity {
	return wsl.Identity{
		Generation:     wsl.WSL2,
		KernelRelease:  "5.15.167.4-microsoft-standard-WSL2",
		WSLVersion:     wsl.Observation{Value: "2.7.12"},
		Distribution:   wsl.Observation{Value: "Ubuntu-24.04"},
		WindowsBuild:   wsl.Observation{Value: "26100"},
		NetworkingMode: wsl.Observation{Value: "nat"},
		NATPrefix:      wsl.Observation{Value: "172.19.144.0/20"},
	}
}

func renderedWSLFinding(t *testing.T, identity wsl.Identity) (doctorFinding, string) {
	t.Helper()
	finding, ok := wslDoctorFinding(identity)
	if !ok {
		t.Fatal("wslDoctorFinding() absent for WSL identity")
	}
	var output bytes.Buffer
	if err := writeRuntimeIdentityFinding(&output, finding); err != nil {
		t.Fatal(err)
	}
	return finding, strings.TrimSpace(output.String())
}

func TestWSLDoctorFindingFullSuccess(t *testing.T) {
	t.Parallel()

	finding, line := renderedWSLFinding(t, completeWSLIdentity())
	wantDetail := "WSL2 2.7.12; distro Ubuntu-24.04; Windows build 26100; networking mode nat; NAT prefix 172.19.144.0/20"
	if finding.level != "INFO" || finding.check != "wsl" || finding.detail != wantDetail {
		t.Fatalf("finding = %+v", finding)
	}
	if want := "INFO wsl: " + wantDetail; line != want {
		t.Fatalf("line = %q, want %q", line, want)
	}
}

func TestWSLDoctorFindingDegradedFields(t *testing.T) {
	t.Parallel()

	unreadable := wsl.Observation{Err: errors.New("unavailable")}
	tests := []struct {
		name   string
		mutate func(*wsl.Identity)
		want   string
	}{
		{name: "version", mutate: func(i *wsl.Identity) { i.WSLVersion = unreadable }, want: "INFO wsl: WSL2 version unreadable; distro Ubuntu-24.04; Windows build 26100; networking mode nat; NAT prefix 172.19.144.0/20"},
		{name: "distro", mutate: func(i *wsl.Identity) { i.Distribution = unreadable }, want: "INFO wsl: WSL2 2.7.12; distro unreadable; Windows build 26100; networking mode nat; NAT prefix 172.19.144.0/20"},
		{name: "Windows side", mutate: func(i *wsl.Identity) { i.WindowsBuild = unreadable }, want: "INFO wsl: WSL2 2.7.12; distro Ubuntu-24.04; Windows side unreadable; networking mode nat; NAT prefix 172.19.144.0/20"},
		{name: "networking mode", mutate: func(i *wsl.Identity) { i.NetworkingMode = unreadable }, want: "INFO wsl: WSL2 2.7.12; distro Ubuntu-24.04; Windows build 26100; networking mode unreadable; NAT prefix 172.19.144.0/20"},
		{name: "NAT prefix", mutate: func(i *wsl.Identity) { i.NATPrefix = unreadable }, want: "INFO wsl: WSL2 2.7.12; distro Ubuntu-24.04; Windows build 26100; networking mode nat; NAT prefix unreadable"},
		{name: "mirrored mode", mutate: func(i *wsl.Identity) { i.NetworkingMode.Value = "mirrored"; i.NATPrefix.Value = "not applicable" }, want: "INFO wsl: WSL2 2.7.12; distro Ubuntu-24.04; Windows build 26100; networking mode mirrored; NAT prefix not applicable"},
		{name: "WSL1", mutate: func(i *wsl.Identity) { i.Generation = wsl.WSL1; i.NATPrefix.Value = "not applicable" }, want: "INFO wsl: WSL1 2.7.12; distro Ubuntu-24.04; Windows build 26100; networking mode nat; NAT prefix not applicable"},
		{name: "all fields", mutate: func(i *wsl.Identity) {
			i.WSLVersion = unreadable
			i.Distribution = unreadable
			i.WindowsBuild = unreadable
			i.NetworkingMode = unreadable
			i.NATPrefix = unreadable
		}, want: "INFO wsl: WSL2 version unreadable; distro unreadable; Windows side unreadable; networking mode unreadable; NAT prefix unreadable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			identity := completeWSLIdentity()
			tt.mutate(&identity)
			_, line := renderedWSLFinding(t, identity)
			if line != tt.want {
				t.Fatalf("line = %q, want %q", line, tt.want)
			}
		})
	}
}

func TestWSLDoctorFindingNeverEscalates(t *testing.T) {
	t.Parallel()

	identities := []wsl.Identity{completeWSLIdentity()}
	for index := 0; index < 5; index++ {
		identity := completeWSLIdentity()
		failed := wsl.Observation{Err: errors.New("unavailable")}
		switch index {
		case 0:
			identity.WSLVersion = failed
		case 1:
			identity.Distribution = failed
		case 2:
			identity.WindowsBuild = failed
		case 3:
			identity.NetworkingMode = failed
		case 4:
			identity.NATPrefix = failed
		}
		identities = append(identities, identity)
	}
	for _, identity := range identities {
		finding, _ := renderedWSLFinding(t, identity)
		if finding.level != "INFO" || strings.Contains(finding.detail, "PASS") || strings.Contains(finding.detail, "WARN") || strings.Contains(finding.detail, "FAIL") {
			t.Fatalf("finding = %+v, want INFO-only inventory", finding)
		}
	}
}

func TestWSLDoctorFindingAbsentOutsideWSL(t *testing.T) {
	t.Parallel()

	if finding, ok := wslDoctorFinding(wsl.Identity{Generation: wsl.NotWSL}); ok {
		t.Fatalf("finding = %+v, want absent", finding)
	}
}
