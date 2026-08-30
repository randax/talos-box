package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/randax/talos-box/internal/cluster"
	"github.com/randax/talos-box/internal/config"
	"github.com/randax/talos-box/internal/hypervisor"
)

const doctorHypervisorPrefix = "INFO Hypervisors: "

const e2eClusterNameMax = 63

var (
	e2eClusterCounter       atomic.Uint64
	e2eClusterStemSanitizer = regexp.MustCompile(`[^a-z0-9]+`)
)

type e2eHypervisorInventory struct {
	Backends map[hypervisor.Name]e2eHypervisorEntry
	Default  hypervisor.Name
}

type e2eHypervisorEntry struct {
	Name          hypervisor.Name
	Available     bool
	Default       bool
	DefaultSource string
	Reason        string
	Remediation   string
	Capabilities  map[string]string
	Raw           string
}

type e2eUnavailableError struct {
	entry e2eHypervisorEntry
}

func (e e2eUnavailableError) Error() string {
	detail := e.entry.Reason
	if e.entry.Remediation != "" {
		detail += "; remediation: " + e.entry.Remediation
	}
	return fmt.Sprintf("hypervisor %q is unavailable: %s", e.entry.Name, detail)
}

func parseDoctorHypervisorInventory(output string, _ error) (e2eHypervisorInventory, error) {
	inventory := e2eHypervisorInventory{Backends: make(map[hypervisor.Name]e2eHypervisorEntry)}
	defaults := 0
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, doctorHypervisorPrefix) {
			continue
		}
		entry, err := parseDoctorHypervisorLine(line)
		if err != nil {
			return e2eHypervisorInventory{}, err
		}
		if _, exists := inventory.Backends[entry.Name]; exists {
			return e2eHypervisorInventory{}, fmt.Errorf("doctor hypervisor inventory has duplicate backend %q", entry.Name)
		}
		inventory.Backends[entry.Name] = entry
		if entry.Default {
			defaults++
			inventory.Default = entry.Name
		}
	}
	if len(inventory.Backends) == 0 {
		return e2eHypervisorInventory{}, errors.New("doctor output has no hypervisor inventory lines")
	}
	if defaults != 1 {
		return e2eHypervisorInventory{}, fmt.Errorf("doctor hypervisor inventory has %d default=yes lines; want exactly one", defaults)
	}
	return inventory, nil
}

func parseDoctorHypervisorLine(line string) (e2eHypervisorEntry, error) {
	raw := strings.TrimPrefix(line, doctorHypervisorPrefix)
	nameText, fieldsText, ok := strings.Cut(raw, ": ")
	if !ok {
		return e2eHypervisorEntry{}, fmt.Errorf("malformed doctor hypervisor line %q", line)
	}
	name, err := hypervisor.ParseName(nameText)
	if err != nil {
		return e2eHypervisorEntry{}, fmt.Errorf("malformed doctor hypervisor line %q: %w", line, err)
	}
	fields, err := splitDoctorFields(fieldsText)
	if err != nil {
		return e2eHypervisorEntry{}, fmt.Errorf("malformed doctor hypervisor line %q: %w", line, err)
	}
	entry := e2eHypervisorEntry{Name: name, Capabilities: make(map[string]string), Raw: line}
	seen := make(map[string]bool)
	for _, field := range fields {
		key, value, ok := strings.Cut(field, "=")
		if !ok || key == "" || value == "" || seen[key] {
			return e2eHypervisorEntry{}, fmt.Errorf("malformed doctor hypervisor field %q", field)
		}
		seen[key] = true
		switch key {
		case "availability":
			status, detail, err := doctorFieldDetail(value)
			if err != nil || (status != "available" && status != "unavailable") {
				return e2eHypervisorEntry{}, fmt.Errorf("malformed availability %q", value)
			}
			entry.Available = status == "available"
			if !entry.Available {
				entry.Reason, entry.Remediation, err = splitUnavailableDetail(detail)
				if err != nil {
					return e2eHypervisorEntry{}, err
				}
			}
		case "default":
			status, detail, err := doctorFieldDetail(value)
			if err != nil || (status != "yes" && status != "no") {
				return e2eHypervisorEntry{}, fmt.Errorf("malformed default %q", value)
			}
			entry.Default = status == "yes"
			if entry.Default {
				const sourcePrefix = "source="
				if !strings.HasPrefix(detail, sourcePrefix) {
					return e2eHypervisorEntry{}, fmt.Errorf("default=yes is missing its source in %q", value)
				}
				entry.DefaultSource = strings.TrimPrefix(detail, sourcePrefix)
				if entry.DefaultSource != string(hypervisor.DefaultSourceCompiled) && entry.DefaultSource != hypervisor.DefaultEnv {
					return e2eHypervisorEntry{}, fmt.Errorf("unknown default source %q", entry.DefaultSource)
				}
			} else if detail != "" {
				return e2eHypervisorEntry{}, fmt.Errorf("default=no unexpectedly has detail %q", detail)
			}
		case "balloon-readback", "suspend", "suspend-survives-restart", "guest-agent":
			entry.Capabilities[key] = value
		default:
			return e2eHypervisorEntry{}, fmt.Errorf("unknown doctor hypervisor field %q", key)
		}
	}
	for _, required := range []string{"availability", "default", "balloon-readback", "suspend", "suspend-survives-restart", "guest-agent"} {
		if !seen[required] {
			return e2eHypervisorEntry{}, fmt.Errorf("doctor hypervisor %q is missing %s", name, required)
		}
	}
	return entry, nil
}

func splitDoctorFields(text string) ([]string, error) {
	var fields []string
	start, depth := 0, 0
	for index, char := range text {
		switch char {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return nil, errors.New("unbalanced parentheses")
			}
		case ';':
			if depth == 0 {
				field := strings.TrimSpace(text[start:index])
				if field == "" {
					return nil, errors.New("empty field")
				}
				fields = append(fields, field)
				start = index + 1
			}
		}
	}
	if depth != 0 {
		return nil, errors.New("unbalanced parentheses")
	}
	last := strings.TrimSpace(text[start:])
	if last == "" {
		return nil, errors.New("empty field")
	}
	return append(fields, last), nil
}

func doctorFieldDetail(value string) (string, string, error) {
	open := strings.IndexByte(value, '(')
	if open < 0 {
		return strings.TrimSpace(value), "", nil
	}
	if !strings.HasSuffix(value, ")") {
		return "", "", errors.New("unterminated field detail")
	}
	status := strings.TrimSpace(value[:open])
	detail := strings.TrimSpace(value[open+1 : len(value)-1])
	if status == "" || detail == "" {
		return "", "", errors.New("empty field status or detail")
	}
	return status, detail, nil
}

func splitUnavailableDetail(detail string) (string, string, error) {
	const marker = "; remediation: "
	reason, remediation, ok := strings.Cut(detail, marker)
	if !ok || strings.TrimSpace(reason) == "" || strings.TrimSpace(remediation) == "" {
		return "", "", fmt.Errorf("unavailable backend detail %q must include reason and remediation", detail)
	}
	return strings.TrimSpace(reason), strings.TrimSpace(remediation), nil
}

func selectE2EHypervisor(raw string, inventory e2eHypervisorInventory) (hypervisor.Name, error) {
	if strings.TrimSpace(raw) == "" {
		if inventory.Default == "" {
			return "", errors.New("doctor hypervisor inventory has no unique default")
		}
		return inventory.Default, nil
	}
	name, err := hypervisor.ParseName(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("TBX_E2E_HYPERVISOR: %w", err)
	}
	return name, nil
}

func selectedE2EHypervisor(inventory e2eHypervisorInventory, name hypervisor.Name) (e2eHypervisorEntry, error) {
	entry, ok := inventory.Backends[name]
	if !ok {
		return e2eHypervisorEntry{}, fmt.Errorf("doctor hypervisor inventory is missing selected backend %q", name)
	}
	if !entry.Available {
		return e2eHypervisorEntry{}, e2eUnavailableError{entry: entry}
	}
	return entry, nil
}

func validateE2EDoctorPreconditions(output string) error {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "FAIL runtime-compat:") || strings.HasPrefix(line, "FAIL helper:") {
			return fmt.Errorf("e2e precondition from doctor: %s", line)
		}
	}
	return nil
}

func renderE2EConfig(cfg config.Config) (string, error) {
	return renderE2EConfigWithParser(cfg, config.Parse)
}

func renderE2EConfigWithParser(cfg config.Config, parse func([]byte) (config.Config, error)) (string, error) {
	if len(cfg.Clusters) == 0 {
		return "", errors.New("e2e config must contain at least one cluster")
	}
	for _, item := range cfg.Clusters {
		if item.Hypervisor == "" {
			return "", fmt.Errorf("e2e cluster %q must pin its hypervisor", item.Name)
		}
	}
	yaml := config.Marshal(cfg)
	parsed, err := parse([]byte(yaml))
	if err != nil {
		return "", fmt.Errorf("self-check generated e2e config: %w", err)
	}
	if len(parsed.Clusters) != len(cfg.Clusters) {
		return "", fmt.Errorf("self-check generated e2e config: got %d clusters, want %d", len(parsed.Clusters), len(cfg.Clusters))
	}
	for index := range cfg.Clusters {
		want, got := cfg.Clusters[index], parsed.Clusters[index]
		if got.Name != want.Name || got.Hypervisor != want.Hypervisor {
			return "", fmt.Errorf("self-check generated e2e config cluster %d: got %q/%q, want %q/%q", index, got.Name, got.Hypervisor, want.Name, want.Hypervisor)
		}
	}
	return yaml, nil
}

func uniqueE2EClusterName(stem string) string {
	stem = strings.ToLower(stem)
	stem = e2eClusterStemSanitizer.ReplaceAllString(stem, "-")
	stem = strings.Trim(stem, "-")
	if stem == "" {
		stem = "e2e"
	}
	suffix := "-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatUint(e2eClusterCounter.Add(1), 10)
	maxStem := e2eClusterNameMax - len(suffix)
	if maxStem < 1 {
		maxStem = 1
	}
	if len(stem) > maxStem {
		stem = strings.TrimRight(stem[:maxStem], "-")
		if stem == "" {
			stem = "e"
		}
	}
	return stem + suffix
}

func validE2ETestConfig(name string, backend hypervisor.Name) config.Config {
	return config.Config{Clusters: []config.ClusterSpec{{
		Name:          name,
		Hypervisor:    backend,
		ControlPlanes: 1,
		Workers:       0,
		Node: cluster.NodeDefaults{
			MemoryMiB: cluster.DefaultMemoryMiB,
			CPUs:      cluster.DefaultCPUs,
			DiskGiB:   cluster.DefaultDiskGiB,
		},
	}}}
}

func availableDoctorLine(name hypervisor.Name, defaultField string) string {
	return fmt.Sprintf("INFO Hypervisors: %s: availability=available; default=%s; balloon-readback=supported; suspend=supported; suspend-survives-restart=supported; guest-agent=supported", name, defaultField)
}

func unavailableDoctorLine(name hypervisor.Name, defaultField string) string {
	return fmt.Sprintf("INFO Hypervisors: %s: availability=unavailable (HVF is unavailable; remediation: install QEMU with HVF support); default=%s; balloon-readback=unavailable; suspend=unavailable; suspend-survives-restart=unavailable; guest-agent=unavailable", name, defaultField)
}

func TestParseDoctorHypervisorInventory(t *testing.T) {
	output := strings.Join([]string{
		"PASS daemon: reachable",
		unavailableDoctorLine(hypervisor.NameQEMU, "no"),
		availableDoctorLine(hypervisor.NameVZ, "yes (source=compiled)"),
	}, "\n")
	inventory, err := parseDoctorHypervisorInventory(output, nil)
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Default != hypervisor.NameVZ || len(inventory.Backends) != 2 {
		t.Fatalf("inventory = %+v, want two backends and vz default", inventory)
	}
	qemu := inventory.Backends[hypervisor.NameQEMU]
	if qemu.Available || qemu.Reason != "HVF is unavailable" || qemu.Remediation != "install QEMU with HVF support" {
		t.Fatalf("qemu entry = %+v, want full unavailable detail", qemu)
	}
}

func TestParseDoctorHypervisorInventoryTrustsTextAfterDoctorFailure(t *testing.T) {
	output := availableDoctorLine(hypervisor.NameVZ, "yes (source=TBX_HYPERVISOR)")
	inventory, err := parseDoctorHypervisorInventory(output, errors.New("doctor exited 1"))
	if err != nil {
		t.Fatalf("well-formed inventory was rejected because doctor failed: %v", err)
	}
	if !inventory.Backends[hypervisor.NameVZ].Available {
		t.Fatal("available inventory entry was not trusted")
	}
}

func TestParseDoctorHypervisorInventoryRejectsInvalidInventory(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{name: "missing default", output: strings.Join([]string{availableDoctorLine(hypervisor.NameVZ, "no"), availableDoctorLine(hypervisor.NameQEMU, "no")}, "\n"), want: "0 default=yes"},
		{name: "duplicate default", output: strings.Join([]string{availableDoctorLine(hypervisor.NameVZ, "yes (source=compiled)"), availableDoctorLine(hypervisor.NameQEMU, "yes (source=compiled)")}, "\n"), want: "2 default=yes"},
		{name: "malformed line", output: "INFO Hypervisors: qemu availability=available", want: "malformed"},
		{name: "missing field", output: "INFO Hypervisors: vz: availability=available; default=yes (source=compiled)", want: "missing balloon-readback"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseDoctorHypervisorInventory(test.output, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want it to contain %q", err, test.want)
			}
		})
	}
}

func TestSelectE2EHypervisor(t *testing.T) {
	inventory, err := parseDoctorHypervisorInventory(strings.Join([]string{
		availableDoctorLine(hypervisor.NameQEMU, "no"),
		availableDoctorLine(hypervisor.NameVZ, "yes (source=compiled)"),
	}, "\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		raw  string
		want hypervisor.Name
	}{
		{name: "unset", raw: "", want: hypervisor.NameVZ},
		{name: "blank", raw: "  ", want: hypervisor.NameVZ},
		{name: "explicit", raw: "qemu", want: hypervisor.NameQEMU},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := selectE2EHypervisor(test.raw, inventory)
			if err != nil || got != test.want {
				t.Fatalf("selectE2EHypervisor(%q) = %q, %v; want %q", test.raw, got, err, test.want)
			}
		})
	}
	if _, err := selectE2EHypervisor("xen", inventory); err == nil || !strings.Contains(err.Error(), "TBX_E2E_HYPERVISOR") {
		t.Fatalf("invalid selector error = %v, want hard configuration error", err)
	}
}

func TestSelectedE2EHypervisorDecision(t *testing.T) {
	inventory, err := parseDoctorHypervisorInventory(strings.Join([]string{
		unavailableDoctorLine(hypervisor.NameQEMU, "no"),
		availableDoctorLine(hypervisor.NameVZ, "yes (source=compiled)"),
	}, "\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if entry, err := selectedE2EHypervisor(inventory, hypervisor.NameVZ); err != nil || !entry.Available {
		t.Fatalf("available backend = %+v, %v; want proceed", entry, err)
	}
	_, err = selectedE2EHypervisor(inventory, hypervisor.NameQEMU)
	var unavailable e2eUnavailableError
	if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "HVF is unavailable; remediation: install QEMU with HVF support") {
		t.Fatalf("unavailable backend error = %v, want skip detail", err)
	}
	missingInventory, parseErr := parseDoctorHypervisorInventory(availableDoctorLine(hypervisor.NameVZ, "yes (source=compiled)"), nil)
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	if _, err := selectedE2EHypervisor(missingInventory, hypervisor.NameQEMU); err == nil || !strings.Contains(err.Error(), "missing selected backend") {
		t.Fatalf("missing backend error = %v, want failure", err)
	}
}

func TestValidateE2EDoctorPreconditions(t *testing.T) {
	for _, line := range []string{
		"FAIL runtime-compat: daemon protocol is stale",
		"FAIL helper: helper is unavailable",
	} {
		if err := validateE2EDoctorPreconditions(line); err == nil || !strings.Contains(err.Error(), line) {
			t.Fatalf("precondition error = %v, want full doctor finding %q", err, line)
		}
	}
	if err := validateE2EDoctorPreconditions("FAIL DNS: unrelated host DNS check failed"); err != nil {
		t.Fatalf("unrelated doctor failure incorrectly rejected valid inventory: %v", err)
	}
}

func TestRenderE2EConfigRoundTripPinsHypervisor(t *testing.T) {
	cfg := validE2ETestConfig("harness", hypervisor.NameQEMU)
	yaml, err := renderE2EConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(yaml, "hypervisor: qemu") {
		t.Fatalf("generated config does not pin qemu:\n%s", yaml)
	}
	parsed, err := config.Parse([]byte(yaml))
	if err != nil || parsed.Clusters[0].Hypervisor != hypervisor.NameQEMU {
		t.Fatalf("round trip hypervisor = %q, %v; want qemu", parsed.Clusters[0].Hypervisor, err)
	}
}

func TestRenderE2EConfigDetectsMissingOrMismatchedHypervisor(t *testing.T) {
	if _, err := renderE2EConfig(validE2ETestConfig("harness", "")); err == nil || !strings.Contains(err.Error(), "must pin") {
		t.Fatalf("missing hypervisor error = %v", err)
	}
	cfg := validE2ETestConfig("harness", hypervisor.NameQEMU)
	_, err := renderE2EConfigWithParser(cfg, func([]byte) (config.Config, error) {
		return validE2ETestConfig("harness", hypervisor.NameVZ), nil
	})
	if err == nil || !strings.Contains(err.Error(), `got "harness"/"vz", want "harness"/"qemu"`) {
		t.Fatalf("mismatch error = %v", err)
	}
}

func TestUniqueE2EClusterName(t *testing.T) {
	seen := make(map[string]bool)
	valid := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	for range 100 {
		name := uniqueE2EClusterName("Console Test With A Very Long Stem That Must Be Trimmed")
		if seen[name] {
			t.Fatalf("duplicate name %q", name)
		}
		if len(name) > 63 || !valid.MatchString(name) {
			t.Fatalf("name %q is not a bounded DNS label", name)
		}
		seen[name] = true
	}
	if got := uniqueE2EClusterName("!!!"); !strings.HasPrefix(got, "e2e-") {
		t.Fatalf("empty sanitized stem = %q, want e2e prefix", got)
	}
}
